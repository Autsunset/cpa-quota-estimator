package main

import (
	"context"
	"database/sql"
	"math"
	"time"
)

func logPrecisionFromEstimate(estimate weightEstimate) float64 {
	if estimate.Low <= 0 || estimate.High <= estimate.Low {
		return .001
	}
	std := (math.Log(estimate.High) - math.Log(estimate.Low)) / (2 * 1.96)
	if std <= 0 {
		return .001
	}
	return 1 / (std * std)
}

func (s *store) seedOnlineCycleScales(ctx context.Context, fit *weightFit) error {
	if fit == nil || !fit.Available {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, cycle := range fit.CycleScales {
		if cycle.Scale.Value <= 0 {
			continue
		}
		var endpoint int64
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(end_event_id),0) FROM quota_segments WHERE account=? AND window=?
			AND cycle_id=? AND regime_reset_at=? AND lag=? AND end_event_id<=?`, cycle.Account, cycle.Window, cycle.CycleID, cycle.RegimeResetAt, fit.Lag, fit.MaxEndEventID).Scan(&endpoint); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO online_cycle_scales(account,window,cycle_id,regime_reset_at,log_scale,precision,last_end_event_id,updated_at)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(account,window,cycle_id,regime_reset_at) DO UPDATE SET
			log_scale=excluded.log_scale,precision=excluded.precision,last_end_event_id=excluded.last_end_event_id,updated_at=excluded.updated_at
			WHERE online_cycle_scales.updated_at<=excluded.updated_at`, cycle.Account, cycle.Window, cycle.CycleID, cycle.RegimeResetAt,
			math.Log(cycle.Scale.Value), logPrecisionFromEstimate(cycle.Scale), endpoint, fit.FittedAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) applyOnlineSegmentsForEndEvent(ctx context.Context, eventID int64, fit *weightFit) error {
	if fit == nil || !fit.Available {
		return nil
	}
	segments, err := s.quotaSegments(ctx, fit.Lag)
	if err != nil {
		return err
	}
	for _, segment := range segments {
		if segment.EndEventID != eventID || !segment.eligible() {
			continue
		}
		equivalent := learnedSegmentEquivalent(segment, *fit)
		if equivalent <= 0 {
			continue
		}
		var logScale, precision float64
		var previousEnd int64
		err = s.db.QueryRowContext(ctx, `SELECT log_scale,precision,last_end_event_id FROM online_cycle_scales
			WHERE account=? AND window=? AND cycle_id=? AND regime_reset_at=?`, segment.Account, segment.Window, segment.CycleID, segment.RegimeResetAt).
			Scan(&logScale, &precision, &previousEnd)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == sql.ErrNoRows {
			var previous float64
			priorErr := s.db.QueryRowContext(ctx, `SELECT log_scale FROM online_cycle_scales WHERE account=? AND window=?
				ORDER BY updated_at DESC LIMIT 1`, segment.Account, segment.Window).Scan(&previous)
			if priorErr != nil && priorErr != sql.ErrNoRows {
				return priorErr
			}
			if priorErr == nil {
				logScale = previous
			}
			if priorErr == sql.ErrNoRows {
				if scale, ok := learnedScale(fit, segment.Account, segment.Window, segment.CycleID, segment.RegimeResetAt); ok {
					logScale = math.Log(scale)
				}
			}
			sigma := fit.RandomWalkSigma
			if sigma <= 0 {
				sigma = .35
			}
			precision = 1 / (sigma * sigma)
		}
		if previousEnd >= segment.EndEventID {
			continue
		}
		state := onlineCycleScale{LogValue: logScale, Precision: precision}
		state.update(equivalent, segment.DP, segment.BoundaryWeight, 0)
		_, err = s.db.ExecContext(ctx, `INSERT INTO online_cycle_scales(account,window,cycle_id,regime_reset_at,log_scale,precision,last_end_event_id,updated_at)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(account,window,cycle_id,regime_reset_at) DO UPDATE SET
			log_scale=excluded.log_scale,precision=excluded.precision,last_end_event_id=excluded.last_end_event_id,updated_at=excluded.updated_at`,
			segment.Account, segment.Window, segment.CycleID, segment.RegimeResetAt, state.LogValue, state.Precision, segment.EndEventID, time.Now().Unix())
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *store) learnedQuotaAttributionLive(ctx context.Context, fit *weightFit, account, window, model, tier string, resetAt int64, d usageDetail, longThreshold int64) *float64 {
	value, ok := learnedEquivalentForUsage(fit, model, d, tier, longThreshold)
	if !ok {
		return nil
	}
	var logScale float64
	var err error
	if resetAt > 0 {
		err = s.db.QueryRowContext(ctx, `SELECT log_scale FROM online_cycle_scales WHERE account=? AND window=?
			AND regime_reset_at BETWEEN ? AND ? ORDER BY updated_at DESC LIMIT 1`, account, window,
			resetAt-segmentResetSlack, resetAt+segmentResetSlack).Scan(&logScale)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT log_scale FROM online_cycle_scales WHERE account=? AND window=?
			ORDER BY updated_at DESC LIMIT 1`, account, window).Scan(&logScale)
	}
	if err == sql.ErrNoRows {
		return learnedQuotaAttribution(fit, account, window, model, tier, 0, resetAt, d, longThreshold)
	}
	if err != nil {
		return nil
	}
	result := value * math.Exp(logScale)
	return &result
}

func (s *store) overlayOnlineCycleScales(ctx context.Context, fit weightFit) (weightFit, error) {
	if !fit.Available {
		return fit, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT account,window,cycle_id,regime_reset_at,log_scale,precision FROM online_cycle_scales`)
	if err != nil {
		return fit, err
	}
	defer rows.Close()
	for rows.Next() {
		var account, window string
		var cycleID, resetAt int64
		var logScale, precision float64
		if err = rows.Scan(&account, &window, &cycleID, &resetAt, &logScale, &precision); err != nil {
			return fit, err
		}
		std := 0.0
		if precision > 0 {
			std = math.Sqrt(1 / precision)
		}
		scale := weightEstimate{Value: math.Exp(logScale), Low: math.Exp(logScale - 1.96*std), High: math.Exp(logScale + 1.96*std)}
		credits := weightEstimate{Value: 100 / scale.Value, Low: 100 / scale.High, High: 100 / scale.Low}
		found := false
		for i := range fit.CycleScales {
			cycle := &fit.CycleScales[i]
			if cycle.Account == account && cycle.Window == window && cycle.CycleID == cycleID && cycle.RegimeResetAt == resetAt {
				cycle.Scale, cycle.CreditsPerPercent = scale, credits
				found = true
				break
			}
		}
		if !found {
			fit.CycleScales = append(fit.CycleScales, learnedCycleScale{Account: account, Window: window, CycleID: cycleID, RegimeResetAt: resetAt, Scale: scale, CreditsPerPercent: credits})
		}
	}
	return fit, rows.Err()
}
