package main

import (
	"context"
	"database/sql"
)

// A fresh full window provides additional reset evidence. Only that case can
// confirm a run above 5%, and its readings must still be at most half the old
// peak. The same-schedule rule remains unchanged.
func earlyResetPercentCandidate(peak, used float64, advanced bool) bool {
	return resetCandidate(peak, used) || (advanced && peak > 0 && used >= 0 && used <= peak/2)
}

func advancedEarlyResetCandidate(ctx context.Context, tx *sql.Tx, current quotaCycle, e event) (bool, error) {
	if !advancedEarlyResetObservation(current, e) || !earlyResetPercentCandidate(current.PeakPercent, *e.UsedPercent, true) {
		return false, nil
	}
	var lastOriginalObservation int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END),0)
FROM usage_events WHERE cycle_id=? AND quota_scope=? AND failed=0 AND used_percent IS NOT NULL
AND reset_at=? AND window_minutes=?`, current.ID, mainQuotaScope, current.ResetAt, current.WindowMinutes).
		Scan(&lastOriginalObservation)
	if err != nil {
		return false, err
	}
	// A correction to a window which was already running before the latest
	// old-plan response is not evidence of a fresh allocation.
	if lastOriginalObservation == 0 || e.ResetAt-e.WindowMinutes*60 < lastOriginalObservation-scheduledResetTolerance {
		return false, nil
	}
	var knownPlan bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM usage_events WHERE cycle_id=? AND quota_scope=? AND failed=0 AND used_percent IS NOT NULL
AND reset_at=? AND window_minutes=?
AND (CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END)<=?)`,
		current.ID, mainQuotaScope, e.ResetAt, e.WindowMinutes, lastOriginalObservation).Scan(&knownPlan)
	// Restoring a previously observed plan is a regime recovery, including
	// when the restored reset_at happens to be later than the temporary one.
	return !knownPlan, err
}

// recoverAdvancedEarlyReset returns a zero cycle when the event is unrelated.
// A nonzero cycle with sampleQuota=false retains a pending recovery in raw
// events, without changing the active plan on a single possibly stale response.
func (s *store) recoverAdvancedEarlyReset(ctx context.Context, tx *sql.Tx, current quotaCycle, e event) (quotaCycle, bool, error) {
	var previous quotaCycle
	err := tx.QueryRowContext(ctx, `SELECT id,reset_at,window_minutes,plan_type,peak_used_percent
FROM quota_cycles WHERE account=? AND id<? AND ended_at=? AND close_reason='early_reset'
ORDER BY id DESC LIMIT 1`, e.Account, current.ID, current.StartedAt).
		Scan(&previous.ID, &previous.ResetAt, &previous.WindowMinutes, &previous.PlanType, &previous.PeakPercent)
	if err == sql.ErrNoRows {
		return quotaCycle{}, false, nil
	}
	if err != nil {
		return quotaCycle{}, false, err
	}
	if e.ResetAt != previous.ResetAt || e.WindowMinutes != previous.WindowMinutes || current.WindowMinutes != previous.WindowMinutes ||
		!compatiblePlan(previous.PlanType, e.PlanType) || !compatiblePlan(current.PlanType, e.PlanType) ||
		eventObservationTime(e) >= previous.ResetAt || previous.PeakPercent <= 0 || *e.UsedPercent < previous.PeakPercent-resetPercentTolerance {
		return quotaCycle{}, false, nil
	}
	if current.ResetAt == e.ResetAt && current.FirstSampleAt > 0 && current.StartPercent < previous.PeakPercent-resetPercentTolerance {
		return quotaCycle{}, false, nil
	}
	var allocatedReset int64
	err = tx.QueryRowContext(ctx, `SELECT reset_at FROM quota_samples WHERE cycle_id=? ORDER BY sampled_at,id LIMIT 1`, current.ID).Scan(&allocatedReset)
	if err == sql.ErrNoRows {
		return quotaCycle{}, false, nil
	}
	if err != nil {
		return quotaCycle{}, false, err
	}
	if allocatedReset <= previous.ResetAt {
		return quotaCycle{}, false, nil
	}
	if e.Failed {
		return current, false, nil
	}
	first, confirmed, possible, err := confirmOriginalScheduleRecovery(ctx, tx, current.ID, previous.PeakPercent, e)
	if err == nil && !possible {
		return quotaCycle{}, false, nil
	}
	if err != nil || !confirmed {
		return current, false, err
	}
	// The entire operation participates in insertEvent's transaction. Rejoin
	// the evidence before detecting A -> B -> A so the anomaly stays visible.
	if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=? WHERE cycle_id=?`, previous.ID, current.ID); err != nil {
		return quotaCycle{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE quota_samples SET cycle_id=? WHERE cycle_id=?`, previous.ID, current.ID); err != nil {
		return quotaCycle{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM quota_cycles WHERE id=?`, current.ID); err != nil {
		return quotaCycle{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE quota_cycles SET ended_at=0,close_reason='' WHERE id=?`, previous.ID); err != nil {
		return quotaCycle{}, false, err
	}
	if err = refreshCycleDerivedData(ctx, tx, previous.ID); err != nil {
		return quotaCycle{}, false, err
	}
	if err = refreshCycleSampleStatsForRegime(ctx, tx, previous.ID, previous.ResetAt, previous.WindowMinutes); err != nil {
		return quotaCycle{}, false, err
	}
	var sampled bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM quota_samples WHERE cycle_id=? AND sampled_at=? AND reset_at=? AND window_minutes=?)`,
		previous.ID, first.RequestedAt, first.ResetAt, first.WindowMinutes).Scan(&sampled); err != nil {
		return quotaCycle{}, false, err
	}
	if !sampled {
		used := first.UsedPercent
		firstEvent := event{Account: e.Account, RequestedAt: first.RequestedAt, ObservedAt: first.ObservedAt,
			UsedPercent: &used, ResetAt: first.ResetAt, WindowMinutes: first.WindowMinutes, PlanType: first.PlanType}
		if err = insertSampleFromRecordedEvent(ctx, tx, previous, first.ID, firstEvent); err != nil {
			return quotaCycle{}, false, err
		}
	}
	restored, err := openCycle(ctx, tx, e.Account)
	return restored, true, err
}

func confirmOriginalScheduleRecovery(ctx context.Context, tx *sql.Tx, cycleID int64, oldPeak float64, e event) (first recordedQuotaEvent, confirmed, possible bool, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,requested_at,CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,used_percent,reset_at,window_minutes,plan_type,failed
FROM usage_events WHERE cycle_id=? AND quota_scope=? AND used_percent IS NOT NULL AND reset_at>0 AND window_minutes>0
ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END DESC,id DESC`, cycleID, mainQuotaScope)
	if err != nil {
		return recordedQuotaEvent{}, false, false, err
	}
	defer rows.Close()
	count := 1
	newerUsed := *e.UsedPercent
	for rows.Next() {
		var recorded recordedQuotaEvent
		if err = rows.Scan(&recorded.ID, &recorded.RequestedAt, &recorded.ObservedAt, &recorded.UsedPercent, &recorded.ResetAt, &recorded.WindowMinutes, &recorded.PlanType, &recorded.Failed); err != nil {
			return recordedQuotaEvent{}, false, false, err
		}
		if recorded.ObservedAt > eventObservationTime(e) {
			return recordedQuotaEvent{}, false, true, nil
		}
		if !sameQuotaRegime(recorded, e) || recorded.Failed {
			break
		}
		// A run which began at a genuinely refilled percentage must not later
		// become a rollback solely because normal consumption reaches oldPeak.
		if recorded.UsedPercent < oldPeak-resetPercentTolerance || recorded.UsedPercent > newerUsed+resetPercentTolerance {
			return recordedQuotaEvent{}, false, false, nil
		}
		if first.ID == 0 || recorded.RequestedAt < first.RequestedAt || (recorded.RequestedAt == first.RequestedAt && recorded.ID < first.ID) {
			first = recorded
		}
		newerUsed = recorded.UsedPercent
		count++
	}
	return first, count >= quotaRegimeConfirmationSamples, true, rows.Err()
}
