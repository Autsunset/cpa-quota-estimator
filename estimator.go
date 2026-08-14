package main

import (
	"math"
	"sort"
)

func estimateCapacity(points []quotaPoint) estimate {
	result := estimate{Confidence: "insufficient", Explanation: "至少需要两次周额度百分比增长后才能估算。"}
	if len(points) < 2 {
		return result
	}
	var tokenEstimates, costEstimates []float64
	first, last := points[0], points[len(points)-1]
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
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
