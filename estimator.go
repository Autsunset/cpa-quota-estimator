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
	windowStart := last.CycleStart
	if windowStart <= 0 {
		windowStart = last.ResetAt - last.WindowMinutes*60
	}
	windowSeconds := last.ResetAt - windowStart
	if windowSeconds <= 0 {
		windowSeconds = last.WindowMinutes * 60
		windowStart = last.ResetAt - windowSeconds
	}
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
	segments := estimationMilestoneSegments(points)
	if len(segments) > 0 {
		milestones := segments[len(segments)-1].Points
		if len(milestones) < 2 {
			return result
		}
		latest := milestones[len(milestones)-1]
		cutoff := latest.Time - 24*60*60
		anchorIndex := 0
		for i := 0; i < len(milestones)-1; i++ {
			if milestones[i].Time <= cutoff {
				anchorIndex = i
			}
		}
		anchor := milestones[anchorIndex]
		recentSeconds := latest.Time - anchor.Time
		recentSpan := latest.UsedPercent - anchor.UsedPercent
		if recentSeconds > 0 && recentSpan > 0 {
			recentRate := recentSpan / float64(recentSeconds)
			result.RecentAvailable = true
			result.RecentWindowSeconds = recentSeconds
			result.RecentPercentSpan = recentSpan
			result.RecentPercentPerDay = recentRate * 86400
			result.RecentProjectedAtReset = used + recentRate*float64(remaining)
			result.RecentEstimatedExhaustAt = calculatedAt + int64((100-used)/recentRate)
			result.RecentWillExhaustBefore = result.RecentEstimatedExhaustAt < last.ResetAt
		}
	}
	return result
}

func estimateCapacity(points []quotaPoint) estimate {
	result := estimate{Confidence: "insufficient", Explanation: "至少需要一个有效额度增长区间（即两个递增的额度样本）后才能估算。"}
	if len(points) < 2 {
		return result
	}
	// Concurrent responses may carry stale percentages, and periodic samples
	// repeat the same integer percentage. Use only the first crossing of each
	// new all-time high; adjacent periodic samples would severely undercount
	// the work needed to advance by one percent.
	var tokenEstimates, costEstimates []float64
	segments := estimationMilestoneSegments(points)
	var latestSegment []quotaPoint
	for _, segment := range segments {
		milestones := segment.Points
		if len(milestones) > 0 {
			latestSegment = milestones
		}
		if len(milestones) > 1 {
			span := milestones[len(milestones)-1].UsedPercent - milestones[0].UsedPercent
			result.PercentSpan = math.Max(result.PercentSpan, span)
		}
		for i := 1; i < len(milestones); i++ {
			a, b := milestones[i-1], milestones[i]
			dp := b.UsedPercent - a.UsedPercent
			if dp <= 0 {
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
	}
	if len(tokenEstimates) == 0 && len(costEstimates) == 0 {
		return result
	}
	result.Available = true
	result.SampleCount = max(len(tokenEstimates), len(costEstimates))
	result.FullWindowTokens = median(tokenEstimates)
	result.FullWindowCostUSD = median(costEstimates)
	result.TokenLow, result.TokenHigh = quantile(tokenEstimates, .25), quantile(tokenEstimates, .75)
	result.CostLow, result.CostHigh = quantile(costEstimates, .25), quantile(costEstimates, .75)
	current := latestReliablePoint(points)
	remaining := math.Max(0, 100-current.UsedPercent) / 100
	result.RemainingTokens = result.FullWindowTokens * remaining
	result.RemainingCostUSD = result.FullWindowCostUSD * remaining
	result.Confidence = "low"
	result.Explanation = "依据相邻额度增长区间的中位数估算；官方额度并非固定美元或 Token，结果仅表示当前负载结构下的等效容量。"
	if result.SampleCount >= 5 && result.PercentSpan >= 5 {
		result.Confidence = "high"
	} else if result.SampleCount >= 2 && result.PercentSpan >= 2 {
		result.Confidence = "medium"
	}
	if len(latestSegment) >= 2 {
		first, last := latestSegment[0], latestSegment[len(latestSegment)-1]
		seconds := last.Time - first.Time
		span := last.UsedPercent - first.UsedPercent
		if seconds > 0 && span > 0 {
			rate := span / float64(seconds)
			result.EstimatedExhaustAt = current.Time + int64((100-current.UsedPercent)/rate)
		}
	}
	return result
}

func capacityHistory(points []quotaPoint) []capacityPoint {
	var tokenEstimates, costEstimates []float64
	segments := estimationMilestoneSegments(points)
	out := make([]capacityPoint, 0, len(points))
	for _, segment := range segments {
		anchored := false
		// Capacity remains at the last trustworthy estimate while an anomalous
		// interval is excluded. Emit that carried estimate at the exact recovery
		// boundary so the visual history resumes there without implying that the
		// anomalous observations contributed a new estimate.
		if segment.BreakBefore && len(segment.Points) > 0 && (len(tokenEstimates) > 0 || len(costEstimates) > 0) {
			first := segment.Points[0]
			out = append(out, capacityPoint{
				CycleID:           first.CycleID,
				Time:              first.Time,
				UsedPercent:       first.UsedPercent,
				FullWindowTokens:  median(tokenEstimates),
				FullWindowCostUSD: median(costEstimates),
				SampleCount:       max(len(tokenEstimates), len(costEstimates)),
				BreakBefore:       true,
			})
			anchored = true
		}
		for i := 1; i < len(segment.Points); i++ {
			a, b := segment.Points[i-1], segment.Points[i]
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
				CycleID:           b.CycleID,
				Time:              b.Time,
				UsedPercent:       b.UsedPercent,
				FullWindowTokens:  median(tokenEstimates),
				FullWindowCostUSD: median(costEstimates),
				SampleCount:       max(len(tokenEstimates), len(costEstimates)),
				BreakBefore:       segment.BreakBefore && i == 1 && !anchored,
			})
		}
	}
	return out
}

func capacityHistoryForCycles(points []quotaPoint) []capacityPoint {
	var out []capacityPoint
	for start := 0; start < len(points); {
		end := start + 1
		for end < len(points) && points[end].CycleID == points[start].CycleID {
			end++
		}
		out = append(out, capacityHistory(points[start:end])...)
		start = end
	}
	return out
}

func monotonicMilestones(points []quotaPoint) []quotaPoint {
	segments := estimationMilestoneSegments(points)
	var milestones []quotaPoint
	for _, segment := range segments {
		milestones = append(milestones, segment.Points...)
	}
	return milestones
}

type quotaMilestoneSegment struct {
	Points      []quotaPoint
	BreakBefore bool
}

func estimationMilestoneSegments(points []quotaPoint) []quotaMilestoneSegment {
	var segments []quotaMilestoneSegment
	var current quotaMilestoneSegment
	pendingBreak := false
	flush := func() {
		if len(current.Points) > 0 {
			segments = append(segments, current)
		}
		current = quotaMilestoneSegment{}
	}
	for _, point := range points {
		if point.Anomalous {
			flush()
			pendingBreak = true
			continue
		}
		if point.BreakBefore {
			flush()
			pendingBreak = true
		}
		if len(current.Points) == 0 {
			current = quotaMilestoneSegment{Points: []quotaPoint{point}, BreakBefore: pendingBreak}
			pendingBreak = false
			continue
		}
		if point.UsedPercent > current.Points[len(current.Points)-1].UsedPercent {
			current.Points = append(current.Points, point)
		}
	}
	flush()
	return segments
}

func latestReliablePoint(points []quotaPoint) quotaPoint {
	for index := len(points) - 1; index >= 0; index-- {
		if !points[index].Anomalous {
			return points[index]
		}
	}
	return quotaPoint{}
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
