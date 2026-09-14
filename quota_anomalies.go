package main

import (
	"context"
	"database/sql"
	"sort"
)

const (
	quotaRegimeConfirmationSamples = 2
	quotaRegimeReverted            = "upstream_regime_reverted"
)

type quotaRegimeObservation struct {
	RequestedAt   int64
	UsedPercent   float64
	ResetAt       int64
	WindowMinutes int64
}

type quotaRegimeRun struct {
	ResetAt       int64
	WindowMinutes int64
	StartedAt     int64
	EndedAt       int64
	FirstUsed     float64
	LastUsed      float64
	PeakAt        int64
	PeakUsed      float64
	Count         int64
}

func (s *store) quotaRegimeAnomalies(ctx context.Context, cycleID int64) ([]quotaRegimeAnomaly, error) {
	if cycleID == 0 {
		return []quotaRegimeAnomaly{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT requested_at,used_percent,reset_at,window_minutes
FROM usage_events
WHERE cycle_id=? AND quota_scope=? AND failed=0 AND used_percent IS NOT NULL AND reset_at>0 AND window_minutes>0
ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,id`, cycleID, mainQuotaScope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var observations []quotaRegimeObservation
	for rows.Next() {
		var observation quotaRegimeObservation
		if err = rows.Scan(&observation.RequestedAt, &observation.UsedPercent,
			&observation.ResetAt, &observation.WindowMinutes); err != nil {
			return nil, err
		}
		observations = append(observations, observation)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return detectQuotaRegimeAnomalies(cycleID, observations), nil
}

func (s *store) quotaRegimeAnomaliesForCycles(ctx context.Context, cycles []quotaCycle) ([]quotaRegimeAnomaly, error) {
	anomalies := make([]quotaRegimeAnomaly, 0, len(cycles))
	seen := make(map[int64]bool, len(cycles))
	for _, cycle := range cycles {
		if cycle.ID == 0 || seen[cycle.ID] {
			continue
		}
		seen[cycle.ID] = true
		cycleAnomalies, err := s.quotaRegimeAnomalies(ctx, cycle.ID)
		if err != nil {
			return nil, err
		}
		anomalies = append(anomalies, cycleAnomalies...)
	}
	return anomalies, nil
}

func detectQuotaRegimeAnomalies(cycleID int64, observations []quotaRegimeObservation) []quotaRegimeAnomaly {
	if len(observations) == 0 {
		return []quotaRegimeAnomaly{}
	}
	runs := make([]quotaRegimeRun, 0, 4)
	for _, observation := range observations {
		if len(runs) == 0 || !sameQuotaRegimeKey(runs[len(runs)-1], observation.ResetAt, observation.WindowMinutes) {
			runs = append(runs, quotaRegimeRun{
				ResetAt: observation.ResetAt, WindowMinutes: observation.WindowMinutes,
				StartedAt: observation.RequestedAt, EndedAt: observation.RequestedAt,
				FirstUsed: observation.UsedPercent, LastUsed: observation.UsedPercent,
				PeakAt: observation.RequestedAt, PeakUsed: observation.UsedPercent, Count: 1,
			})
			continue
		}
		run := &runs[len(runs)-1]
		run.EndedAt = observation.RequestedAt
		run.LastUsed = observation.UsedPercent
		if observation.UsedPercent > run.PeakUsed {
			run.PeakAt = observation.RequestedAt
			run.PeakUsed = observation.UsedPercent
		}
		run.Count++
	}

	// A single concurrent response can carry stale headers. Only expose state
	// changes that were independently repeated, and merge matching runs that
	// become adjacent after those one-off responses are discarded.
	confirmed := make([]quotaRegimeRun, 0, len(runs))
	for index, run := range runs {
		if run.Count < quotaRegimeConfirmationSamples {
			// The first retained response can be the only pre-change sample.
			// A later repeated restoration corroborates that original plan;
			// temporary and restored runs still require repeated observations.
			corroborated := false
			if index == 0 {
				for _, later := range runs[index+1:] {
					if later.Count >= quotaRegimeConfirmationSamples && sameQuotaRegimeKey(later, run.ResetAt, run.WindowMinutes) {
						corroborated = true
						break
					}
				}
			}
			if !corroborated {
				continue
			}
		}
		if len(confirmed) > 0 && sameQuotaRegimeKey(confirmed[len(confirmed)-1], run.ResetAt, run.WindowMinutes) {
			previous := &confirmed[len(confirmed)-1]
			previous.EndedAt = run.EndedAt
			previous.LastUsed = run.LastUsed
			if run.PeakUsed > previous.PeakUsed {
				previous.PeakAt = run.PeakAt
				previous.PeakUsed = run.PeakUsed
			}
			previous.Count += run.Count
			continue
		}
		confirmed = append(confirmed, run)
	}

	anomalies := make([]quotaRegimeAnomaly, 0, 2)
	for index := 1; index+1 < len(confirmed); index++ {
		before, anomalous, restored := confirmed[index-1], confirmed[index], confirmed[index+1]
		if sameQuotaRegimeKey(before, anomalous.ResetAt, anomalous.WindowMinutes) ||
			!sameQuotaRegimeKey(before, restored.ResetAt, restored.WindowMinutes) {
			continue
		}
		anomalies = append(anomalies, quotaRegimeAnomaly{
			CycleID: cycleID, Kind: quotaRegimeReverted,
			BeforeAt: before.EndedAt, StartedAt: anomalous.StartedAt,
			PeakAt: anomalous.PeakAt, EndedAt: restored.StartedAt,
			BeforeUsedPercent: before.LastUsed, AnomalousUsedPercent: anomalous.FirstUsed,
			PeakUsedPercent:     anomalous.PeakUsed,
			RestoredUsedPercent: restored.FirstUsed, BeforeResetAt: before.ResetAt,
			AnomalousResetAt: anomalous.ResetAt, RestoredResetAt: restored.ResetAt,
			ObservationCount: anomalous.Count,
		})
	}
	return anomalies
}

func (s *store) addQuotaAnomalyBoundaryPoints(ctx context.Context, cycle quotaCycle, points []quotaPoint, anomalies []quotaRegimeAnomaly) ([]quotaPoint, error) {
	for _, anomaly := range anomalies {
		boundaries := []quotaPoint{
			{Time: anomaly.BeforeAt, UsedPercent: anomaly.BeforeUsedPercent, ResetAt: anomaly.BeforeResetAt, WindowMinutes: cycle.WindowMinutes},
			{Time: anomaly.StartedAt, UsedPercent: anomaly.AnomalousUsedPercent, ResetAt: anomaly.AnomalousResetAt, WindowMinutes: cycle.WindowMinutes, Anomalous: true, BreakBefore: true},
			{Time: anomaly.PeakAt, UsedPercent: anomaly.PeakUsedPercent, ResetAt: anomaly.AnomalousResetAt, WindowMinutes: cycle.WindowMinutes, Anomalous: true},
			{Time: anomaly.EndedAt, UsedPercent: anomaly.RestoredUsedPercent, ResetAt: anomaly.RestoredResetAt, WindowMinutes: cycle.WindowMinutes, BreakBefore: true},
		}
		for _, boundary := range boundaries {
			boundary.CycleID = cycle.ID
			boundary.CycleStart = cycle.StartedAt
			var found bool
			for index := range points {
				if points[index].Time == boundary.Time && points[index].ResetAt == boundary.ResetAt {
					points[index].UsedPercent = boundary.UsedPercent
					points[index].Anomalous = points[index].Anomalous || boundary.Anomalous
					points[index].BreakBefore = points[index].BreakBefore || boundary.BreakBefore
					found = true
					break
				}
			}
			if found {
				continue
			}
			if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*)
FROM usage_events WHERE cycle_id=? AND quota_scope=? AND requested_at<=?`, cycle.ID, mainQuotaScope, boundary.Time).
				Scan(&boundary.WindowTokens, &boundary.WindowCostUSD, &boundary.Requests); err != nil {
				return nil, err
			}
			points = append(points, boundary)
		}
	}
	sort.SliceStable(points, func(left, right int) bool {
		return points[left].Time < points[right].Time
	})
	return points, nil
}

func sameQuotaRegimeKey(run quotaRegimeRun, resetAt, windowMinutes int64) bool {
	return run.ResetAt == resetAt && run.WindowMinutes == windowMinutes
}

func markQuotaAnomalyPoints(points []quotaPoint, anomalies []quotaRegimeAnomaly) {
	for anomalyIndex := range anomalies {
		anomaly := anomalies[anomalyIndex]
		markedStart := false
		markedRecovery := false
		for pointIndex := range points {
			point := &points[pointIndex]
			if point.Time >= anomaly.StartedAt && point.Time < anomaly.EndedAt &&
				point.ResetAt == anomaly.AnomalousResetAt {
				point.Anomalous = true
				if !markedStart {
					point.BreakBefore = true
					markedStart = true
				}
			}
			if !markedRecovery && point.Time >= anomaly.EndedAt &&
				point.ResetAt == anomaly.RestoredResetAt {
				point.BreakBefore = true
				markedRecovery = true
			}
		}
	}
}

func refreshCycleSampleStatsForRegime(ctx context.Context, tx *sql.Tx, cycleID, resetAt, windowMinutes int64) error {
	var firstAt, lastAt int64
	var firstUsed, lastUsed, peakUsed sql.NullFloat64
	err := tx.QueryRowContext(ctx, `SELECT
	COALESCE(MIN(sampled_at),0),COALESCE(MAX(sampled_at),0),
	(SELECT used_percent FROM quota_samples WHERE cycle_id=? AND reset_at=? AND window_minutes=? ORDER BY sampled_at,id LIMIT 1),
	(SELECT used_percent FROM quota_samples WHERE cycle_id=? AND reset_at=? AND window_minutes=? ORDER BY sampled_at DESC,id DESC LIMIT 1),
	MAX(used_percent)
FROM quota_samples WHERE cycle_id=? AND reset_at=? AND window_minutes=?`,
		cycleID, resetAt, windowMinutes, cycleID, resetAt, windowMinutes,
		cycleID, resetAt, windowMinutes).
		Scan(&firstAt, &lastAt, &firstUsed, &lastUsed, &peakUsed)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE quota_cycles SET
	first_sample_at=?,last_sample_at=?,start_used_percent=?,end_used_percent=?,peak_used_percent=?
WHERE id=?`, firstAt, lastAt, nullableFloat(firstUsed), nullableFloat(lastUsed), nullableFloat(peakUsed), cycleID)
	return err
}

func nullableFloat(value sql.NullFloat64) float64 {
	if value.Valid {
		return value.Float64
	}
	return 0
}

func (s *store) reconcileOpenCycleQuotaRegimes() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id,reset_at,window_minutes FROM quota_cycles WHERE ended_at=0`)
	if err != nil {
		return err
	}
	type openRegime struct{ id, resetAt, windowMinutes int64 }
	var regimes []openRegime
	for rows.Next() {
		var regime openRegime
		if err = rows.Scan(&regime.id, &regime.resetAt, &regime.windowMinutes); err != nil {
			rows.Close()
			return err
		}
		regimes = append(regimes, regime)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, regime := range regimes {
		if err = refreshCycleSampleStatsForRegime(context.Background(), tx, regime.id, regime.resetAt, regime.windowMinutes); err != nil {
			return err
		}
	}
	return tx.Commit()
}
