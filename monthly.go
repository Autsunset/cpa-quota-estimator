package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *store) months(ctx context.Context, account string, limit int) ([]string, error) {
	if limit <= 0 || limit > 120 {
		limit = 36
	}
	var firstEvent, firstCycle int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(requested_at),0) FROM usage_events WHERE account=?`, account).Scan(&firstEvent); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(started_at),0) FROM quota_cycles WHERE account=?`, account).Scan(&firstCycle); err != nil {
		return nil, err
	}
	earliest := firstEvent
	if earliest == 0 || firstCycle > 0 && firstCycle < earliest {
		earliest = firstCycle
	}
	location := shanghaiLocation()
	now := time.Now().In(location)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
	if earliest == 0 {
		return []string{month.Format("2006-01")}, nil
	}
	first := time.Unix(earliest, 0).In(location)
	firstMonth := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, location)
	months := make([]string, 0, limit)
	for len(months) < limit && !month.Before(firstMonth) {
		months = append(months, month.Format("2006-01"))
		month = month.AddDate(0, -1, 0)
	}
	return months, nil
}

func monthRange(raw string) (string, int64, int64, error) {
	location := shanghaiLocation()
	if raw == "" {
		now := time.Now().In(location)
		raw = fmt.Sprintf("%04d-%02d", now.Year(), int(now.Month()))
	}
	month, err := time.ParseInLocation("2006-01", raw, location)
	if err != nil || month.Format("2006-01") != raw {
		return "", 0, 0, fmt.Errorf("invalid month %q; expected YYYY-MM", raw)
	}
	return raw, month.Unix(), month.AddDate(0, 1, 0).Unix(), nil
}

func (s *store) monthly(ctx context.Context, account, rawMonth string) (monthlySummary, error) {
	month, startAt, endAt, err := monthRange(rawMonth)
	if err != nil {
		return monthlySummary{}, err
	}
	result := monthlySummary{Month: month, Timezone: "Asia/Shanghai", StartAt: startAt, EndAt: endAt, QuotaCoverageComplete: true}
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*) FROM usage_events WHERE account=? AND requested_at>=? AND requested_at<?`, account, startAt, endAt).
		Scan(&result.ActualTokens, &result.ActualCostUSD, &result.Requests); err != nil {
		return result, err
	}

	cycles, err := s.cycles(ctx, account, 1000)
	if err != nil {
		return result, err
	}
	for _, cycle := range cycles {
		cycleEnd := cycle.EndedAt
		if cycleEnd == 0 {
			now := time.Now().Unix()
			if cycle.ResetAt > cycle.StartedAt && cycle.ResetAt < now {
				cycleEnd = cycle.ResetAt
			} else {
				cycleEnd = now + 1
			}
		}
		if cycle.StartedAt >= endAt || cycleEnd <= startAt {
			continue
		}
		item := monthlyCycle{quotaCycle: cycle}
		if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*) FROM usage_events WHERE cycle_id=? AND requested_at>=? AND requested_at<?`, cycle.ID, startAt, endAt).
			Scan(&item.MonthTokens, &item.MonthCostUSD, &item.MonthRequests); err != nil {
			return result, err
		}
		points, _, errPoints := s.pointsForCycle(ctx, account, cycle.ID, 10000)
		if errPoints != nil && errPoints != sql.ErrNoRows {
			return result, errPoints
		}
		estimate := estimateCapacity(points)
		item.CapacityAvailable = estimate.Available
		item.FullWindowTokens = estimate.FullWindowTokens
		item.FullWindowCostUSD = estimate.FullWindowCostUSD
		item.TokenLow, item.TokenHigh = estimate.TokenLow, estimate.TokenHigh
		item.CostLow, item.CostHigh = estimate.CostLow, estimate.CostHigh
		item.Confidence = estimate.Confidence
		result.Cycles = append(result.Cycles, item)
		result.CycleCount++

		consumed, complete, errGrowth := s.cycleQuotaGrowth(ctx, cycle, startAt, endAt)
		if errGrowth != nil {
			return result, errGrowth
		}
		result.ConsumedQuotaPercent += consumed
		result.QuotaCoverageComplete = result.QuotaCoverageComplete && complete

		if cycle.EndedAt >= startAt && cycle.EndedAt < endAt && isResetCloseReason(cycle.CloseReason) {
			result.ResetCount++
			if cycle.CloseReason == "early_reset" {
				result.EarlyResetCount++
			}
			result.UnusedQuotaAtReset += maxFloat(0, 100-cycle.PeakPercent)
		}
		if cycle.StartedAt >= startAt && cycle.StartedAt < endAt {
			result.AllocatedCycleCount++
			if estimate.Available {
				result.EstimatedCycleCount++
				result.EstimatedTokens += estimate.FullWindowTokens
				result.EstimatedTokenLow += estimate.TokenLow
				result.EstimatedTokenHigh += estimate.TokenHigh
				result.EstimatedCostUSD += estimate.FullWindowCostUSD
				result.EstimatedCostLow += estimate.CostLow
				result.EstimatedCostHigh += estimate.CostHigh
			}
		}
	}
	result.ConsumedQuotaEquivalent = result.ConsumedQuotaPercent / 100
	return result, nil
}

func isResetCloseReason(reason string) bool {
	return reason == "scheduled_reset" || reason == "early_reset" || reason == "observed_reset"
}

func (s *store) cycleQuotaGrowth(ctx context.Context, cycle quotaCycle, startAt, endAt int64) (float64, bool, error) {
	var endPeak sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(used_percent) FROM quota_samples WHERE cycle_id=? AND sampled_at<?`, cycle.ID, endAt).Scan(&endPeak); err != nil {
		return 0, false, err
	}
	if !endPeak.Valid {
		return 0, false, nil
	}
	if cycle.StartedAt >= startAt {
		// The upstream percentage is cumulative from the cycle reset. Once the
		// reset boundary is known, the peak directly describes consumption in
		// this month even if the first observed request was not near 0%.
		return maxFloat(0, endPeak.Float64), true, nil
	}
	var baseline sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(used_percent) FROM quota_samples WHERE cycle_id=? AND sampled_at<?`, cycle.ID, startAt).Scan(&baseline); err != nil {
		return 0, false, err
	}
	if baseline.Valid {
		return maxFloat(0, endPeak.Float64-baseline.Float64), true, nil
	}
	var firstInMonth sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT used_percent FROM quota_samples WHERE cycle_id=? AND sampled_at>=? AND sampled_at<? ORDER BY sampled_at ASC LIMIT 1`, cycle.ID, startAt, endAt).Scan(&firstInMonth); err != nil && err != sql.ErrNoRows {
		return 0, false, err
	}
	if firstInMonth.Valid {
		return maxFloat(0, endPeak.Float64-firstInMonth.Float64), false, nil
	}
	return 0, false, nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
