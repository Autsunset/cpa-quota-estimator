package main

import (
	"context"
	"database/sql"
	"fmt"
)

const (
	falseResetMinimumPeak       = 20.0
	falseResetMinimumChain      = 2
	falseResetReboundMaxSeconds = 10 * 60
	missedResetRepairKey        = "missed_advanced_early_resets_v1"
)

type missedResetBoundary struct {
	CycleID       int64
	EventID       int64
	At            int64
	OldReset      int64
	NewReset      int64
	WindowMinutes int64
	PlanType      string
}

// repairMissedAdvancedEarlyResets upgrades cycles created before advanced
// early-reset detection existed. A confirmed low run in a new weekly schedule
// is required; the entire repair and its marker commit atomically.
func (s *store) repairMissedAdvancedEarlyResets(ctx context.Context) ([]missedResetBoundary, error) {
	var marker string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, missedResetRepairKey).Scan(&marker)
	if err == nil && marker == "complete" {
		return nil, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,started_at,ended_at FROM quota_cycles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	type cycleStart struct{ id, started, ended int64 }
	var cycles []cycleStart
	for rows.Next() {
		var c cycleStart
		if err = rows.Scan(&c.id, &c.started, &c.ended); err != nil {
			rows.Close()
			return nil, err
		}
		cycles = append(cycles, c)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var boundaries []missedResetBoundary
	for _, cycle := range cycles {
		found, errFind := s.missedResetInCycle(ctx, cycle.id, cycle.started, cycle.ended)
		if errFind != nil {
			return nil, errFind
		}
		if found.EventID != 0 {
			boundaries = append(boundaries, found)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, b := range boundaries {
		var account, reason string
		var ended int64
		if err = tx.QueryRowContext(ctx, `SELECT account,ended_at,close_reason FROM quota_cycles WHERE id=?`, b.CycleID).Scan(&account, &ended, &reason); err != nil {
			return nil, err
		}
		result, errInsert := tx.ExecContext(ctx, `INSERT INTO quota_cycles(account,started_at,ended_at,reset_at,window_minutes,plan_type,close_reason)
			VALUES(?,?,?,?,?,?,?)`, account, b.At, ended, b.NewReset, b.WindowMinutes, b.PlanType, reason)
		if errInsert != nil {
			return nil, errInsert
		}
		newID, errID := result.LastInsertId()
		if errID != nil {
			return nil, errID
		}
		if _, err = tx.ExecContext(ctx, `UPDATE quota_cycles SET ended_at=?,reset_at=?,close_reason='early_reset' WHERE id=?`, b.At, b.OldReset, b.CycleID); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=? WHERE cycle_id=? AND quota_scope=? AND (requested_at>? OR (requested_at=? AND id>=?))`, newID, b.CycleID, mainQuotaScope, b.At, b.At, b.EventID); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE quota_samples SET cycle_id=? WHERE cycle_id=? AND sampled_at>=? AND reset_at>=?`, newID, b.CycleID, b.At, b.NewReset-segmentResetSlack); err != nil {
			return nil, err
		}
		var firstUsed float64
		if err = tx.QueryRowContext(ctx, `SELECT used_percent FROM usage_events WHERE id=?`, b.EventID).Scan(&firstUsed); err != nil {
			return nil, err
		}
		firstEvent := event{Account: account, RequestedAt: b.At, ObservedAt: b.At, UsedPercent: &firstUsed, ResetAt: b.NewReset, WindowMinutes: b.WindowMinutes, PlanType: b.PlanType}
		if err = insertSampleFromRecordedEvent(ctx, tx, quotaCycle{ID: newID}, b.EventID, firstEvent); err != nil {
			return nil, err
		}
		if err = refreshCycleDerivedData(ctx, tx, b.CycleID); err != nil {
			return nil, err
		}
		if err = refreshCycleDerivedData(ctx, tx, newID); err != nil {
			return nil, err
		}
	}
	if len(boundaries) > 0 {
		// Rebuild segments and fits after migrateSegments has ensured their schema.
		if _, err = tx.ExecContext(ctx, `DELETE FROM metadata WHERE key=?`, segmentBackfillKey); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM weight_fits; DELETE FROM online_cycle_scales`); err != nil {
			return nil, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?, 'complete') ON CONFLICT(key) DO UPDATE SET value='complete'`, missedResetRepairKey); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return boundaries, nil
}

func (s *store) missedResetInCycle(ctx context.Context, cycleID, startedAt, endedAt int64) (missedResetBoundary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,requested_at,CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,used_percent,reset_at,window_minutes,plan_type,failed
		FROM usage_events WHERE cycle_id=? AND quota_scope=? AND used_percent IS NOT NULL AND reset_at>0
		ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,id`, cycleID, mainQuotaScope)
	if err != nil {
		return missedResetBoundary{}, err
	}
	defer rows.Close()
	var events []recordedQuotaEvent
	for rows.Next() {
		var e recordedQuotaEvent
		if err = rows.Scan(&e.ID, &e.RequestedAt, &e.ObservedAt, &e.UsedPercent, &e.ResetAt, &e.WindowMinutes, &e.PlanType, &e.Failed); err != nil {
			return missedResetBoundary{}, err
		}
		events = append(events, e)
	}
	if err = rows.Err(); err != nil {
		return missedResetBoundary{}, err
	}
	for i := 1; i < len(events); i++ {
		old, first := events[i-1], events[i]
		if old.Failed || first.Failed || old.UsedPercent < 20 || first.UsedPercent > resetLowPercent || first.ResetAt <= old.ResetAt+scheduledResetTolerance || first.WindowMinutes != old.WindowMinutes || !compatiblePlan(first.PlanType, old.PlanType) {
			continue
		}
		declared := first.ResetAt - first.WindowMinutes*60
		// A low immediately before an already recorded boundary belongs to that
		// boundary; it is not evidence that a long merged cycle needs splitting.
		if endedAt > 0 && endedAt-first.RequestedAt < 3600 {
			continue
		}
		if declared <= startedAt || absInt64(declared-first.ObservedAt) > scheduledResetTolerance || first.ObservedAt >= old.ResetAt-scheduledResetTolerance {
			continue
		}
		count, lastAt := 1, first.ObservedAt
		for j := i + 1; j < len(events); j++ {
			next := events[j]
			if next.ObservedAt-first.ObservedAt > 10*60 {
				break
			}
			if next.Failed || absInt64(next.ResetAt-first.ResetAt) > segmentResetSlack || next.UsedPercent > resetLowPercent+resetPercentTolerance {
				break
			}
			count++
			lastAt = next.ObservedAt
		}
		if count >= resetConfirmationSamples && lastAt-first.ObservedAt >= resetConfirmationMinSeconds {
			return missedResetBoundary{CycleID: cycleID, EventID: first.ID, At: first.RequestedAt, OldReset: old.ResetAt, NewReset: first.ResetAt, WindowMinutes: first.WindowMinutes, PlanType: first.PlanType}, nil
		}
	}
	return missedResetBoundary{}, nil
}

type earlyResetRepairCandidate struct {
	FromCycleID   int64 `json:"from_cycle_id"`
	IntoCycleID   int64 `json:"into_cycle_id"`
	BoundaryAt    int64 `json:"boundary_at"`
	LowAt         int64 `json:"low_observed_at"`
	ReboundAt     int64 `json:"rebound_at"`
	SeparateQuota bool  `json:"separate_quota_scope,omitempty"`

	PreviousPeak float64 `json:"previous_peak_percent"`
	Reason       string  `json:"reason"`
}

type earlyResetRepairReport struct {
	Account        string                      `json:"account"`
	Applied        bool                        `json:"applied"`
	CandidateCount int                         `json:"candidate_count"`
	MergedCount    int                         `json:"merged_count"`
	Candidates     []earlyResetRepairCandidate `json:"candidates"`
}

func (s *store) repairFalseEarlyResets(ctx context.Context, account string, apply bool) (earlyResetRepairReport, error) {
	report := earlyResetRepairReport{Account: account, Applied: apply, Candidates: []earlyResetRepairCandidate{}}
	if account == "" {
		return report, fmt.Errorf("account is required")
	}
	cycles, err := s.cycles(ctx, account, 1000)
	if err != nil {
		return report, err
	}
	for left, right := 0, len(cycles)-1; left < right; left, right = left+1, right-1 {
		cycles[left], cycles[right] = cycles[right], cycles[left]
	}
	suspects := make([]earlyResetRepairCandidate, 0)
	for index := 0; index+1 < len(cycles); index++ {
		previous, next := cycles[index], cycles[index+1]
		if previous.CloseReason != "early_reset" || previous.EndedAt == 0 || previous.PeakPercent < falseResetMinimumPeak {
			continue
		}
		if previous.ResetAt != next.ResetAt || previous.WindowMinutes != next.WindowMinutes || !compatiblePlan(previous.PlanType, next.PlanType) {
			continue
		}
		separateLowAt, separateReboundAt, separateQuota, errSeparate := s.historicalSeparateQuotaReset(ctx, previous, next)
		if errSeparate != nil {
			return report, errSeparate
		}
		if separateQuota {
			suspects = append(suspects, earlyResetRepairCandidate{
				FromCycleID:   previous.ID,
				IntoCycleID:   next.ID,
				BoundaryAt:    previous.EndedAt,
				LowAt:         separateLowAt,
				PreviousPeak:  previous.PeakPercent,
				ReboundAt:     separateReboundAt,
				SeparateQuota: true,
				Reason:        "a Spark-only quota reading created the boundary, then the main quota immediately continued at the previous usage level",
			})
			continue
		}
		confirmed, firstLowAt, errEvidence := s.historicalEarlyResetConfirmed(ctx, previous, next)
		if errEvidence != nil {
			return report, errEvidence
		}
		if confirmed || firstLowAt == 0 {
			continue
		}
		reboundAt, errRebound := s.earlyResetReboundAt(ctx, next.ID, previous.PeakPercent)
		if errRebound != nil {
			return report, errRebound
		}
		if reboundAt == 0 || reboundAt < firstLowAt || reboundAt-firstLowAt > falseResetReboundMaxSeconds {
			continue
		}
		suspects = append(suspects, earlyResetRepairCandidate{
			FromCycleID:  previous.ID,
			IntoCycleID:  next.ID,
			BoundaryAt:   previous.EndedAt,
			LowAt:        firstLowAt,
			PreviousPeak: previous.PeakPercent,
			ReboundAt:    reboundAt,
			Reason:       "the initial low reading was not confirmed by three stable observations over 60 seconds before usage rebounded to the previous peak",
		})
	}
	for start := 0; start < len(suspects); {
		end := start + 1
		for end < len(suspects) && suspects[end-1].IntoCycleID == suspects[end].FromCycleID {
			end++
		}
		if end-start >= falseResetMinimumChain {
			report.Candidates = append(report.Candidates, suspects[start:end]...)
		}
		start = end
	}
	report.CandidateCount = len(report.Candidates)
	if !apply || len(report.Candidates) == 0 {
		return report, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	survivors := make(map[int64]struct{})
	for index, candidate := range report.Candidates {
		if err = mergeCycleIntoNext(ctx, tx, candidate); err != nil {
			return report, err
		}
		report.MergedCount++
		if index+1 == len(report.Candidates) || candidate.IntoCycleID != report.Candidates[index+1].FromCycleID {
			survivors[candidate.IntoCycleID] = struct{}{}
		}
	}
	if err = detachSeparateQuotaEvents(ctx, tx, account); err != nil {
		return report, err
	}
	for cycleID := range survivors {
		if err = refreshCycleDerivedData(ctx, tx, cycleID); err != nil {
			return report, err
		}
	}
	if err = tx.Commit(); err != nil {
		return report, err
	}
	return report, nil
}

func (s *store) historicalSeparateQuotaReset(ctx context.Context, previous, next quotaCycle) (int64, int64, bool, error) {
	var lowID, lowAt int64
	err := s.db.QueryRowContext(ctx, `SELECT id,CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END
FROM usage_events
WHERE cycle_id=? AND quota_scope<>? AND used_percent IS NOT NULL AND used_percent<=? AND reset_at>0 AND window_minutes>0
ORDER BY id
LIMIT 1`, next.ID, mainQuotaScope, resetLowPercent).Scan(&lowID, &lowAt)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	var reboundAt int64
	err = s.db.QueryRowContext(ctx, `SELECT CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END
FROM usage_events
WHERE cycle_id=? AND quota_scope=? AND id>? AND failed=0 AND used_percent>=?
AND reset_at=? AND window_minutes=?
AND (?='' OR plan_type='' OR plan_type=?)
AND ABS((CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END)-?)<=?
ORDER BY id
LIMIT 1`, next.ID, mainQuotaScope, lowID, previous.PeakPercent-resetPercentTolerance,
		previous.ResetAt, previous.WindowMinutes, previous.PlanType, previous.PlanType,
		lowAt, falseResetReboundMaxSeconds).Scan(&reboundAt)
	if err == sql.ErrNoRows {
		return lowAt, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return lowAt, reboundAt, true, nil
}

func (s *store) historicalEarlyResetConfirmed(ctx context.Context, previous, next quotaCycle) (bool, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,requested_at,CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,used_percent,reset_at,window_minutes,plan_type,failed
FROM usage_events
WHERE cycle_id=? AND quota_scope=? AND used_percent IS NOT NULL AND reset_at>0 AND window_minutes>0
ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,id`, next.ID, mainQuotaScope)
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	var firstObserved int64
	var previousUsed float64
	count := 0
	for rows.Next() {
		var item recordedQuotaEvent
		if err = rows.Scan(&item.ID, &item.RequestedAt, &item.ObservedAt, &item.UsedPercent, &item.ResetAt, &item.WindowMinutes, &item.PlanType, &item.Failed); err != nil {
			return false, 0, err
		}
		if item.Failed || item.ResetAt != next.ResetAt || item.WindowMinutes != next.WindowMinutes || !compatiblePlan(item.PlanType, next.PlanType) || !earlyResetPercentCandidate(previous.PeakPercent, item.UsedPercent, previous.ResetAt != next.ResetAt) {
			return false, firstObserved, nil
		}
		if count > 0 && item.UsedPercent+resetPercentTolerance < previousUsed {
			return false, firstObserved, nil
		}
		if count == 0 {
			firstObserved = item.ObservedAt
		}
		previousUsed = item.UsedPercent
		count++
		if count >= resetConfirmationSamples && item.ObservedAt-firstObserved >= resetConfirmationMinSeconds {
			return true, firstObserved, nil
		}
	}
	return false, firstObserved, rows.Err()
}

func (s *store) earlyResetReboundAt(ctx context.Context, cycleID int64, previousPeak float64) (int64, error) {
	var reboundAt int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END),0)
FROM usage_events
WHERE cycle_id=? AND quota_scope=? AND failed=0 AND used_percent>=?`, cycleID, mainQuotaScope, previousPeak-resetPercentTolerance).Scan(&reboundAt)
	return reboundAt, err
}

func mergeCycleIntoNext(ctx context.Context, tx *sql.Tx, candidate earlyResetRepairCandidate) error {
	var fromEnd, fromReset, fromWindow, intoStart, intoReset, intoWindow int64
	var fromReason, fromPlan, intoPlan string
	if err := tx.QueryRowContext(ctx, `SELECT previous.ended_at,previous.reset_at,previous.window_minutes,previous.plan_type,previous.close_reason,
next.started_at,next.reset_at,next.window_minutes,next.plan_type
FROM quota_cycles previous
JOIN quota_cycles next ON next.id=?
WHERE previous.id=?`, candidate.IntoCycleID, candidate.FromCycleID).
		Scan(&fromEnd, &fromReset, &fromWindow, &fromPlan, &fromReason, &intoStart, &intoReset, &intoWindow, &intoPlan); err != nil {
		return err
	}
	if fromReason != "early_reset" || fromEnd != candidate.BoundaryAt || intoStart != candidate.BoundaryAt || fromReset != intoReset || fromWindow != intoWindow || !compatiblePlan(fromPlan, intoPlan) {
		return fmt.Errorf("repair candidate %d -> %d is no longer a compatible early-reset boundary", candidate.FromCycleID, candidate.IntoCycleID)
	}
	if !candidate.SeparateQuota {
		if _, err := tx.ExecContext(ctx, `DELETE FROM quota_samples WHERE cycle_id=? AND sampled_at>=? AND sampled_at<=? AND used_percent<=?`, candidate.IntoCycleID, candidate.BoundaryAt-1, candidate.ReboundAt, resetLowPercent); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=? WHERE cycle_id=?`, candidate.IntoCycleID, candidate.FromCycleID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE quota_samples SET cycle_id=? WHERE cycle_id=?`, candidate.IntoCycleID, candidate.FromCycleID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE quota_cycles SET started_at=(SELECT started_at FROM quota_cycles WHERE id=?) WHERE id=?`, candidate.FromCycleID, candidate.IntoCycleID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM quota_cycles WHERE id=?`, candidate.FromCycleID); err != nil {
		return err
	}
	return nil
}

func detachSeparateQuotaEvents(ctx context.Context, tx *sql.Tx, account string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM quota_samples
WHERE account=? AND EXISTS (
	SELECT 1 FROM usage_events
	WHERE usage_events.account=quota_samples.account
	AND usage_events.quota_scope<>?
	AND usage_events.requested_at=quota_samples.sampled_at
	AND usage_events.reset_at=quota_samples.reset_at
	AND usage_events.used_percent IS NOT NULL
	AND ABS(usage_events.used_percent-quota_samples.used_percent)<0.000001
)`, account, mainQuotaScope); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE usage_events SET cycle_id=0 WHERE account=? AND quota_scope<>?`, account, mainQuotaScope)
	return err
}

func refreshCycleDerivedData(ctx context.Context, tx *sql.Tx, cycleID int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE quota_samples
SET window_tokens=(SELECT COALESCE(SUM(total_tokens),0) FROM usage_events WHERE usage_events.cycle_id=quota_samples.cycle_id AND usage_events.quota_scope='main' AND usage_events.requested_at<=quota_samples.sampled_at),
window_cost_usd=(SELECT COALESCE(SUM(cost_usd),0) FROM usage_events WHERE usage_events.cycle_id=quota_samples.cycle_id AND usage_events.quota_scope='main' AND usage_events.requested_at<=quota_samples.sampled_at),
requests=(SELECT COUNT(*) FROM usage_events WHERE usage_events.cycle_id=quota_samples.cycle_id AND usage_events.quota_scope='main' AND usage_events.requested_at<=quota_samples.sampled_at)
WHERE cycle_id=?`, cycleID); err != nil {
		return err
	}
	return refreshCycleSampleSummary(ctx, tx, cycleID)
}

func refreshCycleSampleSummary(ctx context.Context, tx *sql.Tx, cycleID int64) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_samples WHERE cycle_id=?`, cycleID).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		_, err := tx.ExecContext(ctx, `UPDATE quota_cycles SET first_sample_at=0,last_sample_at=0,start_used_percent=0,end_used_percent=0,peak_used_percent=0 WHERE id=?`, cycleID)
		return err
	}
	var firstAt, lastAt int64
	var startUsed, endUsed, peak float64
	if err := tx.QueryRowContext(ctx, `SELECT sampled_at,used_percent FROM quota_samples WHERE cycle_id=? ORDER BY sampled_at,id LIMIT 1`, cycleID).Scan(&firstAt, &startUsed); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT sampled_at,used_percent FROM quota_samples WHERE cycle_id=? ORDER BY sampled_at DESC,id DESC LIMIT 1`, cycleID).Scan(&lastAt, &endUsed); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT MAX(used_percent) FROM quota_samples WHERE cycle_id=?`, cycleID).Scan(&peak); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE quota_cycles SET first_sample_at=?,last_sample_at=?,start_used_percent=?,end_used_percent=?,peak_used_percent=? WHERE id=?`, firstAt, lastAt, startUsed, endUsed, peak, cycleID)
	return err
}

func compatiblePlan(left, right string) bool {
	return left == "" || right == "" || left == right
}
