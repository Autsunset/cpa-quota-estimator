package main

import (
	"math"
	"sort"
)

func estimateBurn(points []quotaPoint, now int64) burnForecast {
	result := burnForecast{Status: "insufficient"}
	if len(points) == 0 {
		return result
	}
	last := points[len(points)-1]
	windowSeconds := last.WindowMinutes * 60
	windowStart := last.ResetAt - windowSeconds
	if windowSeconds <= 0 || last.ResetAt <= windowStart || now < windowStart {
		return result
	}
	calculatedAt := min(now, last.ResetAt)
	elapsed := calculatedAt - windowStart
	if elapsed <= 0 {
		return result
	}
	remaining := max(int64(0), last.ResetAt-calculatedAt)
	timeProgress := math.Min(100, float64(elapsed)*100/float64(windowSeconds))
	used := math.Max(0, last.UsedPercent)
	paceRatio := float64(0)
	if timeProgress > 0 {
		paceRatio = used / timeProgress
	}
	averagePerDay := used * 86400 / float64(elapsed)
	sustainablePerDay := 100 * 86400 / float64(windowSeconds)
	projectedAtReset := paceRatio * 100
	status := "on_track"
	delta := used - timeProgress
	if delta > 2 {
		status = "fast"
	} else if delta < -2 {
		status = "slow"
	}
	result = burnForecast{
		Available:                true,
		WindowStart:              windowStart,
		ResetAt:                  last.ResetAt,
		CalculatedAt:             calculatedAt,
		WindowSeconds:            windowSeconds,
		ElapsedSeconds:           elapsed,
		RemainingSeconds:         remaining,
		TimeProgressPercent:      timeProgress,
		UsedPercent:              used,
		ExpectedUsedPercent:      timeProgress,
		PaceDeltaPercent:         delta,
		PaceRatio:                paceRatio,
		AveragePercentPerDay:     averagePerDay,
		SustainablePercentPerDay: sustainablePerDay,
		ProjectedUsedAtReset:     projectedAtReset,
		Status:                   status,
	}
	if used > 0 {
		result.EstimatedExhaustAt = windowStart + int64(float64(elapsed)*100/used)
		result.WillExhaustBeforeReset = result.EstimatedExhaustAt < last.ResetAt
	}
	return result
}

func estimateCapacity(points []quotaPoint) estimate {
	result := estimate{Confidence: "insufficient", Explanation: "至少需要两次周额度百分比增长后才能估算。"}
	if len(points) < 2 {
		return result
	}
	// Concurrent responses may carry stale percentages, and periodic samples
	// repeat the same integer percentage. Use only the first crossing of each
	// new all-time high; adjacent periodic samples would severely undercount
	// the work needed to advance by one percent.
	milestones := monotonicMilestones(points)
	if len(milestones) < 2 {
		return result
	}
	var tokenEstimates, costEstimates []float64
	first, last := milestones[0], milestones[len(milestones)-1]
	for i := 1; i < len(milestones); i++ {
		a, b := milestones[i-1], milestones[i]
		dp := b.UsedPercent - a.UsedPercent
		if dp <= 0 || b.ResetAt != a.ResetAt {
			continue
		}
		dt := float64(b.WindowTokens - a.WindowTokens)
		dc := b.WindowCostUSD - a.WindowCostUSD
		if dt > 0 {
			tokenEstimates = append(tokenEstimates, dt*100/dp)
		}
		if dc > 0 {
			costEstimates = append(costEstimates, dc*100/dp)
		}
	}
	if len(tokenEstimates) == 0 && len(costEstimates) == 0 {
		return result
	}
	result.Available = true
	result.PercentSpan = last.UsedPercent - first.UsedPercent
	result.SampleCount = max(len(tokenEstimates), len(costEstimates))
	result.FullWindowTokens = median(tokenEstimates)
	result.FullWindowCostUSD = median(costEstimates)
	result.TokenLow, result.TokenHigh = quantile(tokenEstimates, .25), quantile(tokenEstimates, .75)
	result.CostLow, result.CostHigh = quantile(costEstimates, .25), quantile(costEstimates, .75)
	remaining := math.Max(0, 100-last.UsedPercent) / 100
	result.RemainingTokens = result.FullWindowTokens * remaining
	result.RemainingCostUSD = result.FullWindowCostUSD * remaining
	result.Confidence = "low"
	result.Explanation = "依据相邻额度增长区间的中位数估算；官方额度并非固定美元或 Token，结果仅表示当前负载结构下的等效容量。"
	if result.SampleCount >= 5 && result.PercentSpan >= 5 {
		result.Confidence = "high"
	} else if result.SampleCount >= 2 && result.PercentSpan >= 2 {
		result.Confidence = "medium"
	}
	seconds := last.Time - first.Time
	if seconds > 0 && result.PercentSpan > 0 {
		rate := result.PercentSpan / float64(seconds)
		result.EstimatedExhaustAt = last.Time + int64((100-last.UsedPercent)/rate)
	}
	return result
}

func capacityHistory(points []quotaPoint) []capacityPoint {
	milestones := monotonicMilestones(points)
	if len(milestones) < 2 {
		return []capacityPoint{}
	}
	var tokenEstimates, costEstimates []float64
	out := make([]capacityPoint, 0, len(milestones)-1)
	for i := 1; i < len(milestones); i++ {
		a, b := milestones[i-1], milestones[i]
		dp := b.UsedPercent - a.UsedPercent
		if dp <= 0 {
			continue
		}
		if delta := float64(b.WindowTokens - a.WindowTokens); delta > 0 {
			tokenEstimates = append(tokenEstimates, delta*100/dp)
		}
		if delta := b.WindowCostUSD - a.WindowCostUSD; delta > 0 {
			costEstimates = append(costEstimates, delta*100/dp)
		}
		out = append(out, capacityPoint{
			Time:              b.Time,
			UsedPercent:       b.UsedPercent,
			FullWindowTokens:  median(tokenEstimates),
			FullWindowCostUSD: median(costEstimates),
			SampleCount:       max(len(tokenEstimates), len(costEstimates)),
		})
	}
	return out
}

func monotonicMilestones(points []quotaPoint) []quotaPoint {
	if len(points) == 0 {
		return nil
	}
	milestones := []quotaPoint{points[0]}
	maxPercent := points[0].UsedPercent
	for _, point := range points[1:] {
		if point.ResetAt == points[0].ResetAt && point.UsedPercent > maxPercent {
			milestones = append(milestones, point)
			maxPercent = point.UsedPercent
		}
	}
	return milestones
}

func median(values []float64) float64 { return quantile(values, .5) }

func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	x := append([]float64(nil), values...)
	sort.Float64s(x)
	if len(x) == 1 {
		return x[0]
	}
	pos := q * float64(len(x)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return x[lo]
	}
	return x[lo] + (x[hi]-x[lo])*(pos-float64(lo))
}
