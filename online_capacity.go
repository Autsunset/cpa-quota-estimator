package main

import (
	"context"
	"math"
	"strconv"
	"strings"
)

func burnWithOnlineCapacity(points []quotaPoint, now int64, capacity estimate) burnForecast {
	forecast := estimateBurn(points, now)
	if capacity.Method != "online_cycle_scale" || !capacity.Available || capacity.FullWindowCostUSD <= 0 || len(points) < 2 {
		return forecast
	}
	if segments := estimationMilestoneSegments(points); len(segments) > 0 {
		points = segments[len(segments)-1].Points
		if len(points) < 2 {
			return forecast
		}
	}
	first, last := points[0], points[len(points)-1]
	seconds := last.Time - first.Time
	spent := last.WindowCostUSD - first.WindowCostUSD
	if seconds <= 0 || spent <= 0 {
		return forecast
	}
	remaining := capacity.RemainingCostUSD
	forecast.EstimatedExhaustAt = now + int64(remaining*float64(seconds)/spent)
	forecast.WillExhaustBeforeReset = forecast.EstimatedExhaustAt < last.ResetAt
	forecast.AveragePercentPerDay = spent / float64(seconds) / (capacity.FullWindowCostUSD / 100) * 86400
	forecast.ProjectedUsedAtReset = last.UsedPercent + float64(max(int64(0), last.ResetAt-now))*spent/float64(seconds)/(capacity.FullWindowCostUSD/100)
	// Recent pace uses the same remaining value and its own observed value rate.
	anchor := first
	for i := len(points) - 2; i >= 0; i-- {
		if points[i].Time <= last.Time-86400 {
			anchor = points[i]
			break
		}
	}
	recentSeconds := last.Time - anchor.Time
	recentValue := last.WindowCostUSD - anchor.WindowCostUSD
	if recentSeconds > 0 && recentValue > 0 {
		forecast.RecentAvailable = true
		forecast.RecentWindowSeconds = recentSeconds
		forecast.RecentPercentSpan = recentValue / (capacity.FullWindowCostUSD / 100)
		forecast.RecentPercentPerDay = forecast.RecentPercentSpan / float64(recentSeconds) * 86400
		forecast.RecentEstimatedExhaustAt = now + int64(remaining*float64(recentSeconds)/recentValue)
		forecast.RecentWillExhaustBefore = forecast.RecentEstimatedExhaustAt < last.ResetAt
		forecast.RecentProjectedAtReset = last.UsedPercent + float64(max(int64(0), last.ResetAt-now))*recentValue/float64(recentSeconds)/(capacity.FullWindowCostUSD/100)
	}
	return forecast
}

// onlineCycleCapacity estimates the selected basis per quota point. Each
// cycle has its own scale, with a log random-walk prior from the prior cycle.
// Only crossings observed by cutoff are allowed into the posterior.
func (s *store) onlineCycleCapacity(ctx context.Context, account string, cycle quotaCycle, points []quotaPoint, cfg config, cutoff int64) (estimate, error) {
	fallback := estimateCapacity(points)
	if cycle.ID == 0 || len(points) == 0 || !cycle.Current {
		return fallback, nil
	}
	mode := normalizePricingMode(cfg.PricingMode)
	lag := 2
	if cfg.LearnedFit != nil && cfg.LearnedFit.Available {
		lag = cfg.LearnedFit.Lag
	}
	segments, err := s.quotaSegments(ctx, lag)
	if err != nil {
		return fallback, err
	}
	priceRows, err := s.listPrices(ctx)
	if err != nil {
		return fallback, err
	}
	prices := make(map[string]price, len(priceRows))
	for _, p := range priceRows {
		prices[normalizeModel(p.Model)] = p
	}
	rate := referenceSolRate(mode, prices)
	if mode == pricingModeLearned {
		rate = 1
	}
	if rate <= 0 {
		return fallback, nil
	}
	sigma := cfg.WeightRandomWalkSigma
	if sigma <= 0 {
		sigma = .35
	}
	tracker := &onlineScaleTracker{ByCycle: make(map[string]*onlineCycleScale), LastByGroup: make(map[string]*onlineCycleScale), RandomWalkSigma: sigma}
	counts := make(map[string]int)
	for _, seg := range segments {
		if seg.Account != account || seg.Window != mainQuotaScope || seg.EndAt > cutoff || !seg.eligible() || seg.DP <= 0 {
			continue
		}
		var equivalent float64
		if mode == pricingModeLearned {
			if cfg.LearnedFit != nil {
				equivalent = learnedSegmentEquivalent(seg, *cfg.LearnedFit)
			}
		} else {
			equivalent = referenceEquivalent(seg, mode, prices)
		}
		if equivalent <= 0 {
			continue
		}
		state := tracker.forSegment(seg)
		state.update(equivalent, seg.DP, seg.BoundaryWeight, 0)
		counts[modelCycle(seg)]++
	}
	var state *onlineCycleScale
	var count int
	prefix := account + "|" + mainQuotaScope + "|"
	for key, candidate := range tracker.ByCycle {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		// The persisted cycle ID, rather than the reset timestamp, is the
		// stable identity; upstream reset timestamps can drift by a minute.
		if strings.HasPrefix(key, prefix+itoa(cycle.ID)+"|") {
			state = candidate
			count += counts[key]
		}
	}
	if state == nil {
		// A new cycle with no crossings borrows the preceding cycle's scale.
		if tracker.LastByGroup[prefix[:len(prefix)-1]] == nil {
			return fallback, nil
		}
		state = tracker.forSegment(quotaSegment{Account: account, Window: mainQuotaScope, CycleID: cycle.ID, RegimeResetAt: cycle.ResetAt})
	}
	if state == nil || !isFinitePositive(math.Exp(state.LogValue)) {
		return fallback, nil
	}
	std := math.Sqrt(1 / math.Max(state.Precision, 1e-9))
	valuePerPercent := rate / math.Exp(state.LogValue)
	result := fallback
	result.Method = "online_cycle_scale"
	result.Available = true
	result.SampleCount = count
	result.FullWindowCostUSD = 100 * valuePerPercent
	result.CostLow = 100 * rate / math.Exp(state.LogValue+1.96*std)
	result.CostHigh = 100 * rate / math.Exp(state.LogValue-1.96*std)
	result.ValuePerPercent = valuePerPercent
	result.ValuePerPercentLow = result.CostLow / 100
	result.ValuePerPercentHigh = result.CostHigh / 100
	result.PercentSpan = math.Max(result.PercentSpan, points[len(points)-1].UsedPercent-points[0].UsedPercent)
	remaining := math.Max(0, 100-points[len(points)-1].UsedPercent) / 100
	result.RemainingCostUSD = result.FullWindowCostUSD * remaining
	if ratio := s.recentTokenValueRatio(ctx, account, cycle.ID, points); ratio > 0 {
		result.FullWindowTokens = result.FullWindowCostUSD * ratio
		result.TokenLow = result.CostLow * ratio
		result.TokenHigh = result.CostHigh * ratio
		result.RemainingTokens = result.FullWindowTokens * remaining
	}
	result.Confidence = "low"
	if count >= 5 {
		result.Confidence = "high"
	} else if count >= 2 {
		result.Confidence = "medium"
	}
	if count == 0 {
		result.Explanation = "本周期尚无额度跨越点；使用上一周期尺度和 σ=0.35 的随机游走先验。"
	} else {
		result.Explanation = "按本周期额度跨越点在线更新每 1% 的计价值；区间包含随机游走先验不确定性。"
	}
	return result, nil
}

func (s *store) recentTokenValueRatio(ctx context.Context, account string, cycleID int64, points []quotaPoint) float64 {
	for i := len(points) - 1; i >= 0; i-- {
		if points[i].WindowCostUSD > 0 && points[i].WindowTokens > 0 {
			return float64(points[i].WindowTokens) / points[i].WindowCostUSD
		}
	}
	var tokens int64
	var value float64
	err := s.db.QueryRowContext(ctx, `SELECT window_tokens,window_cost_usd FROM quota_samples WHERE account=? AND cycle_id<>? AND window_tokens>0 AND window_cost_usd>0 ORDER BY sampled_at DESC LIMIT 1`, account, cycleID).Scan(&tokens, &value)
	if err != nil || value <= 0 {
		return 0
	}
	return float64(tokens) / value
}

func isFinitePositive(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
