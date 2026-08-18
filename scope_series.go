package main

import (
	"context"
	"database/sql"
	"time"
)

type scopedQuotaObservation struct {
	ID            int64
	RequestedAt   int64
	ObservedAt    int64
	UsedPercent   float64
	ResetAt       int64
	WindowMinutes int64
	PlanType      string
}

type scopedCycleDefinition struct {
	Cycle            quotaCycle
	ObservationStart int
	ObservationEnd   int
}

func (s *store) latestQuotaScopeSeries(ctx context.Context, account, scope string, limit int) (scopedQuotaSeries, error) {
	definitions, err := s.quotaScopeCycleDefinitions(ctx, account, scope)
	if err != nil || len(definitions) == 0 {
		return scopedQuotaSeries{Scope: scope, Points: []scopedQuotaPoint{}}, err
	}
	return s.quotaScopeSeriesForCycle(ctx, account, scope, definitions[0], limit)
}

func (s *store) quotaScopeSeries(ctx context.Context, account, scope string, selectedResetAt int64, limit int) (scopedQuotaSeries, error) {
	definitions, err := s.quotaScopeCycleDefinitions(ctx, account, scope)
	if err != nil {
		return scopedQuotaSeries{Scope: scope, Points: []scopedQuotaPoint{}}, err
	}
	for _, definition := range definitions {
		if selectedResetAt == 0 || definition.Cycle.ResetAt == selectedResetAt {
			return s.quotaScopeSeriesForCycle(ctx, account, scope, definition, limit)
		}
	}
	return scopedQuotaSeries{Scope: scope, Points: []scopedQuotaPoint{}}, nil
}

func (s *store) quotaScopeSeriesForCycle(ctx context.Context, account, scope string, definition scopedCycleDefinition, limit int) (scopedQuotaSeries, error) {
	cycle := definition.Cycle
	series := scopedQuotaSeries{
		Scope:         scope,
		StartedAt:     cycle.StartedAt,
		ResetAt:       cycle.ResetAt,
		WindowMinutes: cycle.WindowMinutes,
		PlanType:      cycle.PlanType,
		Points:        []scopedQuotaPoint{},
	}
	if account == "" || scope == "" || cycle.ID == 0 {
		return series, nil
	}
	if limit <= 0 || limit > 10000 {
		limit = 5000
	}
	endAt := scopedCycleEnd(cycle)
	if endAt <= cycle.StartedAt {
		return series, nil
	}
	pointTime := `CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END`
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(used_percent),0),COUNT(*)
FROM usage_events
WHERE account=? AND quota_scope=? AND failed=0 AND used_percent IS NOT NULL
	AND `+pointTime+`>=? AND `+pointTime+`<?`, account, scope, cycle.StartedAt, endAt).
		Scan(&series.UsedPercent, &series.ObservationCount); err != nil {
		return series, err
	}
	rows, err := s.db.QueryContext(ctx, `WITH scoped AS (
	SELECT id,requested_at,CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END AS point_time,
		used_percent,failed,
		SUM(total_tokens) OVER (ORDER BY requested_at,id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS window_tokens,
		SUM(cost_usd) OVER (ORDER BY requested_at,id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS window_cost_usd,
		COUNT(*) OVER (ORDER BY requested_at,id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS requests
	FROM usage_events
	WHERE account=? AND quota_scope=? AND requested_at>=? AND requested_at<?
), quota_points AS (
	SELECT *,
		MAX(used_percent) OVER (ORDER BY point_time,id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS prior_peak,
		ROW_NUMBER() OVER (ORDER BY point_time DESC,id DESC) AS newest
	FROM scoped
	WHERE failed=0 AND used_percent IS NOT NULL AND point_time>=? AND point_time<?
)
SELECT point_time,used_percent,window_tokens,window_cost_usd,requests
FROM quota_points
WHERE prior_peak IS NULL OR used_percent>prior_peak OR newest=1
ORDER BY point_time,id
LIMIT ?`, account, scope, cycle.StartedAt, endAt, cycle.StartedAt, endAt, limit)
	if err != nil {
		return series, err
	}
	defer rows.Close()
	for rows.Next() {
		var point scopedQuotaPoint
		if err = rows.Scan(&point.Time, &point.UsedPercent, &point.WindowTokens, &point.WindowCostUSD, &point.Requests); err != nil {
			return series, err
		}
		point.ResetAt = cycle.ResetAt
		point.WindowMinutes = cycle.WindowMinutes
		point.PlanType = cycle.PlanType
		series.Points = append(series.Points, point)
	}
	if err = rows.Err(); err != nil {
		return series, err
	}
	capacityInput := make([]quotaPoint, 0, len(series.Points))
	for _, point := range series.Points {
		capacityInput = append(capacityInput, quotaPoint{
			CycleID:       cycle.ID,
			CycleStart:    cycle.StartedAt,
			Time:          point.Time,
			UsedPercent:   point.UsedPercent,
			ResetAt:       cycle.ResetAt,
			WindowMinutes: cycle.WindowMinutes,
			WindowTokens:  point.WindowTokens,
			WindowCostUSD: point.WindowCostUSD,
			Requests:      point.Requests,
		})
	}
	series.Estimate = estimateCapacity(capacityInput)
	series.CapacityPoints = capacityHistory(capacityInput)
	return series, nil
}

func (s *store) quotaScopeCycles(ctx context.Context, account, scope string) ([]quotaCycle, error) {
	definitions, err := s.quotaScopeCycleDefinitions(ctx, account, scope)
	if err != nil {
		return nil, err
	}
	cycles := make([]quotaCycle, 0, len(definitions))
	for _, definition := range definitions {
		cycles = append(cycles, definition.Cycle)
	}
	return cycles, nil
}

func (s *store) quotaScopeCycleDefinitions(ctx context.Context, account, scope string) ([]scopedCycleDefinition, error) {
	observations, err := s.quotaScopeObservations(ctx, account, scope)
	if err != nil || len(observations) == 0 {
		return nil, err
	}
	startIndex := 0
	cycleStart := scopedDeclaredStart(observations[0], observations[0].ObservedAt)
	cycleReset := observations[0].ResetAt
	cycleWindow := observations[0].WindowMinutes
	cyclePlan := observations[0].PlanType
	fixedStart := false
	peak := observations[0].UsedPercent
	pendingSchedule := -1
	pendingLow := -1
	pendingLowCount := 0
	pendingLowLast := float64(0)
	definitions := make([]scopedCycleDefinition, 0, 8)

	appendCycle := func(endIndex int, endedAt int64, reason string) {
		if endIndex <= startIndex {
			return
		}
		definition := buildScopedCycleDefinition(observations, startIndex, endIndex, cycleStart, cycleReset, cycleWindow, cyclePlan, endedAt, reason)
		definitions = append(definitions, definition)
	}
	recomputePeak := func(from, through int) float64 {
		value := float64(0)
		for index := from; index <= through && index < len(observations); index++ {
			if observations[index].UsedPercent > value {
				value = observations[index].UsedPercent
			}
		}
		return value
	}

	for index := 1; index < len(observations); index++ {
		observation := observations[index]
		if observation.ResetAt != cycleReset {
			if scopedScheduledTransitionCandidate(cycleReset, observation) {
				if pendingSchedule >= 0 && index == pendingSchedule+1 && sameScopedQuotaRegime(observations[pendingSchedule], observation) {
					oldReset := cycleReset
					appendCycle(pendingSchedule, oldReset, "scheduled_reset")
					startIndex = pendingSchedule
					cycleStart = oldReset
					cycleReset = observation.ResetAt
					cycleWindow = observation.WindowMinutes
					cyclePlan = observation.PlanType
					fixedStart = true
					peak = recomputePeak(startIndex, index)
					pendingSchedule = -1
					pendingLow = -1
					pendingLowCount = 0
					continue
				}
				pendingSchedule = index
				continue
			}
			// A reset timestamp that changes before the old boundary while usage
			// continues is a schedule correction, not an observed reset.
			cycleReset = observation.ResetAt
			cycleWindow = observation.WindowMinutes
			if observation.PlanType != "" {
				cyclePlan = observation.PlanType
			}
			if !fixedStart {
				cycleStart = scopedDeclaredStart(observation, cycleStart)
			}
			pendingSchedule = -1
		} else {
			pendingSchedule = -1
			if observation.WindowMinutes > 0 {
				cycleWindow = observation.WindowMinutes
			}
			if observation.PlanType != "" {
				cyclePlan = observation.PlanType
			}
		}

		if resetCandidate(peak, observation.UsedPercent) {
			if pendingLow < 0 || !sameScopedQuotaRegime(observations[pendingLow], observation) || observation.UsedPercent+resetPercentTolerance < pendingLowLast {
				pendingLow = index
				pendingLowCount = 1
			} else {
				pendingLowCount++
			}
			pendingLowLast = observation.UsedPercent
			if pendingLowCount >= resetConfirmationSamples && observation.ObservedAt-observations[pendingLow].ObservedAt >= resetConfirmationMinSeconds {
				boundary := observations[pendingLow].ObservedAt
				appendCycle(pendingLow, boundary, "early_reset")
				startIndex = pendingLow
				cycleStart = boundary
				cycleReset = observation.ResetAt
				cycleWindow = observation.WindowMinutes
				cyclePlan = observation.PlanType
				fixedStart = true
				peak = recomputePeak(startIndex, index)
				pendingLow = -1
				pendingLowCount = 0
			}
			continue
		}
		pendingLow = -1
		pendingLowCount = 0
		if observation.UsedPercent > peak {
			peak = observation.UsedPercent
		}
	}
	appendCycle(len(observations), 0, "")
	if len(definitions) == 0 {
		return nil, nil
	}
	definitions[len(definitions)-1].Cycle.Current = true
	for left, right := 0, len(definitions)-1; left < right; left, right = left+1, right-1 {
		definitions[left], definitions[right] = definitions[right], definitions[left]
	}
	return definitions, nil
}

func (s *store) quotaScopeObservations(ctx context.Context, account, scope string) ([]scopedQuotaObservation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,requested_at,
	CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,
	used_percent,reset_at,window_minutes,plan_type
FROM usage_events
WHERE account=? AND quota_scope=? AND failed=0
	AND used_percent IS NOT NULL AND reset_at>0 AND window_minutes>0
ORDER BY CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END,id`, account, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := make([]scopedQuotaObservation, 0, 256)
	for rows.Next() {
		var observation scopedQuotaObservation
		if err = rows.Scan(&observation.ID, &observation.RequestedAt, &observation.ObservedAt, &observation.UsedPercent, &observation.ResetAt, &observation.WindowMinutes, &observation.PlanType); err != nil {
			return nil, err
		}
		observations = append(observations, observation)
	}
	return observations, rows.Err()
}

func buildScopedCycleDefinition(observations []scopedQuotaObservation, startIndex, endIndex int, startedAt, resetAt, windowMinutes int64, planType string, endedAt int64, reason string) scopedCycleDefinition {
	first := startIndex
	for first < endIndex && observations[first].ObservedAt < startedAt {
		first++
	}
	if first >= endIndex {
		first = startIndex
	}
	last := endIndex - 1
	cycle := quotaCycle{
		ID:               observations[first].ID,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		ResetAt:          resetAt,
		WindowMinutes:    windowMinutes,
		PlanType:         planType,
		CloseReason:      reason,
		FirstSampleAt:    observations[first].ObservedAt,
		LastSampleAt:     observations[last].ObservedAt,
		StartPercent:     observations[first].UsedPercent,
		EndPercent:       observations[last].UsedPercent,
		PeakPercent:      observations[first].UsedPercent,
		ObservedComplete: observations[first].UsedPercent <= resetLowPercent,
	}
	for index := first + 1; index < endIndex; index++ {
		if endedAt > 0 && observations[index].ObservedAt >= endedAt {
			break
		}
		cycle.EndPercent = observations[index].UsedPercent
		cycle.LastSampleAt = observations[index].ObservedAt
		if observations[index].UsedPercent > cycle.PeakPercent {
			cycle.PeakPercent = observations[index].UsedPercent
		}
	}
	return scopedCycleDefinition{Cycle: cycle, ObservationStart: startIndex, ObservationEnd: endIndex}
}

func scopedDeclaredStart(observation scopedQuotaObservation, fallback int64) int64 {
	startedAt := observation.ResetAt - observation.WindowMinutes*60
	if startedAt <= 0 || startedAt > observation.ObservedAt+60 {
		return fallback
	}
	return startedAt
}

func scopedScheduledTransitionCandidate(currentResetAt int64, observation scopedQuotaObservation) bool {
	if currentResetAt <= 0 || observation.ResetAt <= 0 || observation.WindowMinutes <= 0 {
		return false
	}
	declaredStart := observation.ResetAt - observation.WindowMinutes*60
	return absInt64(declaredStart-currentResetAt) <= scheduledResetTolerance &&
		observation.ObservedAt >= currentResetAt-scheduledResetTolerance
}

func sameScopedQuotaRegime(left, right scopedQuotaObservation) bool {
	if left.ResetAt != right.ResetAt || left.WindowMinutes != right.WindowMinutes {
		return false
	}
	return left.PlanType == "" || right.PlanType == "" || left.PlanType == right.PlanType
}

func scopedCycleEnd(cycle quotaCycle) int64 {
	if cycle.EndedAt > cycle.StartedAt {
		return cycle.EndedAt
	}
	if cycle.ResetAt > cycle.StartedAt {
		return cycle.ResetAt
	}
	return cycle.LastSampleAt + 1
}

func (s *store) monthlyQuotaScope(ctx context.Context, account, scope, rawMonth string) (monthlySummary, error) {
	month, startAt, endAt, err := monthRange(rawMonth)
	if err != nil {
		return monthlySummary{}, err
	}
	result := monthlySummary{Month: month, Timezone: "Asia/Shanghai", StartAt: startAt, EndAt: endAt, QuotaCoverageComplete: true}
	if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*)
FROM usage_events
WHERE account=? AND quota_scope=? AND requested_at>=? AND requested_at<?`, account, scope, startAt, endAt).
		Scan(&result.ActualTokens, &result.ActualCostUSD, &result.Requests); err != nil {
		return result, err
	}
	definitions, err := s.quotaScopeCycleDefinitions(ctx, account, scope)
	if err != nil {
		return result, err
	}
	now := time.Now().Unix()
	for _, definition := range definitions {
		cycle := definition.Cycle
		cycleEnd := scopedCycleEnd(cycle)
		if cycle.Current && cycle.ResetAt > now {
			cycleEnd = now + 1
		}
		if cycle.StartedAt >= endAt || cycleEnd <= startAt {
			continue
		}
		item := monthlyCycle{quotaCycle: cycle}
		usageEnd := scopedCycleEnd(cycle)
		if err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*)
FROM usage_events
WHERE account=? AND quota_scope=? AND requested_at>=? AND requested_at<? AND requested_at>=? AND requested_at<?`,
			account, scope, cycle.StartedAt, usageEnd, startAt, endAt).
			Scan(&item.MonthTokens, &item.MonthCostUSD, &item.MonthRequests); err != nil {
			return result, err
		}
		series, errSeries := s.quotaScopeSeriesForCycle(ctx, account, scope, definition, 10000)
		if errSeries != nil {
			return result, errSeries
		}
		estimate := series.Estimate
		item.CapacityAvailable = estimate.Available
		item.FullWindowTokens = estimate.FullWindowTokens
		item.FullWindowCostUSD = estimate.FullWindowCostUSD
		item.TokenLow, item.TokenHigh = estimate.TokenLow, estimate.TokenHigh
		item.CostLow, item.CostHigh = estimate.CostLow, estimate.CostHigh
		item.Confidence = estimate.Confidence
		result.Cycles = append(result.Cycles, item)
		result.CycleCount++

		consumed, complete, errGrowth := s.quotaScopeCycleGrowth(ctx, account, scope, cycle, startAt, endAt)
		if errGrowth != nil {
			return result, errGrowth
		}
		result.ConsumedQuotaPercent += consumed
		result.QuotaCoverageComplete = result.QuotaCoverageComplete && complete
		if !cycle.Current && cycle.EndedAt >= startAt && cycle.EndedAt < endAt {
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

func (s *store) quotaScopeCycleGrowth(ctx context.Context, account, scope string, cycle quotaCycle, startAt, endAt int64) (float64, bool, error) {
	pointTime := `CASE WHEN observed_at>0 THEN observed_at ELSE requested_at END`
	cycleEnd := scopedCycleEnd(cycle)
	var endPeak sql.NullFloat64
	query := `SELECT MAX(used_percent) FROM usage_events
WHERE account=? AND quota_scope=? AND failed=0 AND used_percent IS NOT NULL
	AND ` + pointTime + `>=? AND ` + pointTime + `<? AND ` + pointTime + `<?`
	if err := s.db.QueryRowContext(ctx, query, account, scope, cycle.StartedAt, endAt, cycleEnd).Scan(&endPeak); err != nil {
		return 0, false, err
	}
	if !endPeak.Valid {
		return 0, false, nil
	}
	if cycle.StartedAt >= startAt {
		return maxFloat(0, endPeak.Float64), true, nil
	}
	var baseline sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, query, account, scope, cycle.StartedAt, startAt, cycleEnd).Scan(&baseline); err != nil {
		return 0, false, err
	}
	if baseline.Valid {
		return maxFloat(0, endPeak.Float64-baseline.Float64), true, nil
	}
	var firstInMonth sql.NullFloat64
	firstQuery := `SELECT used_percent FROM usage_events
WHERE account=? AND quota_scope=? AND failed=0 AND used_percent IS NOT NULL
	AND ` + pointTime + `>=? AND ` + pointTime + `<? AND ` + pointTime + `<? ORDER BY ` + pointTime + `,id LIMIT 1`
	if err := s.db.QueryRowContext(ctx, firstQuery, account, scope, startAt, endAt, cycleEnd).Scan(&firstInMonth); err != nil && err != sql.ErrNoRows {
		return 0, false, err
	}
	if firstInMonth.Valid {
		return maxFloat(0, endPeak.Float64-firstInMonth.Float64), false, nil
	}
	return 0, false, nil
}
