package main

import (
	"math"
	"sort"
)

type backtestScore struct {
	Mode         string  `json:"mode"`
	MAE          float64 `json:"mae"`
	Bias         float64 `json:"bias"`
	SegmentCount int     `json:"segment_count"`
}

type backtestLagResult struct {
	Lag              int             `json:"lag"`
	EligibleSegments int             `json:"eligible_segments"`
	FlaggedSegments  int             `json:"flagged_segments"`
	Scores           []backtestScore `json:"scores"`
}

type weightBacktest struct {
	GeneratedAt   int64               `json:"generated_at"`
	SelectedLag   int                 `json:"selected_lag"`
	LagAmbiguous  bool                `json:"lag_ambiguous"`
	Lags          []backtestLagResult `json:"lags"`
	Scores        []backtestScore     `json:"scores"`
	FittedWeights weightFit           `json:"fitted_weights"`
}

type scoreAccumulator struct {
	absError float64
	bias     float64
	count    int
}

type segmentIdentity struct {
	Account      string
	Window       string
	CycleID      int64
	StartEventID int64
	EndEventID   int64
}

func identityOf(segment quotaSegment) segmentIdentity {
	return segmentIdentity{segment.Account, segment.Window, segment.CycleID, segment.StartEventID, segment.EndEventID}
}

func (a *scoreAccumulator) add(prediction, actual float64) {
	a.absError += math.Abs(prediction - actual)
	a.bias += prediction - actual
	a.count++
}

func (a scoreAccumulator) result(mode string) backtestScore {
	result := backtestScore{Mode: mode, SegmentCount: a.count}
	if a.count > 0 {
		result.MAE = a.absError / float64(a.count)
		result.Bias = a.bias / float64(a.count)
	}
	return result
}

func fitReferenceScales(segments []quotaSegment, mode string, prices map[string]price, now int64, halfLifeDays float64) map[string]float64 {
	groups := make(map[string][]quotaSegment)
	for _, segment := range segments {
		if value := referenceEquivalent(segment, mode, prices); value > 0 {
			groups[modelGroup(segment)] = append(groups[modelGroup(segment)], segment)
		}
	}
	scales := make(map[string]float64)
	for group, items := range groups {
		var ratios []float64
		for _, item := range items {
			ratios = append(ratios, item.DP/referenceEquivalent(item, mode, prices))
		}
		sort.Float64s(ratios)
		scale := ratios[len(ratios)/2]
		for iteration := 0; iteration < 12; iteration++ {
			var numerator, denominator float64
			for _, item := range items {
				value := referenceEquivalent(item, mode, prices)
				residual := item.DP - scale*value
				weight := segmentFitWeight(item, now, halfLifeDays)
				if a := math.Abs(residual); a > weightHuberDelta {
					weight *= weightHuberDelta / a
				}
				numerator += weight * value * item.DP
				denominator += weight * value * value
			}
			if denominator > 0 {
				scale = numerator / denominator
			}
		}
		scales[group] = scale
	}
	return scales
}

func rollingBacktest(byLag map[int][]quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions) (weightBacktest, error) {
	result := weightBacktest{GeneratedAt: now, Lags: []backtestLagResult{}, Scores: []backtestScore{}}
	bestMAE := math.Inf(1)
	secondMAE := math.Inf(1)
	// Compare lags on identical crossings. Otherwise lag 2 can appear better
	// merely because its first (incomplete) segment is excluded.
	common := make(map[segmentIdentity]int)
	for lag := 0; lag <= 2; lag++ {
		seen := make(map[segmentIdentity]bool)
		for _, segment := range byLag[lag] {
			if segment.eligible() && segment.DP > 0 && referenceEquivalent(segment, pricingModeCredits, prices) > 0 {
				seen[identityOf(segment)] = true
			}
		}
		for id := range seen {
			common[id]++
		}
	}
	for lag := 0; lag <= 2; lag++ {
		all := append([]quotaSegment(nil), byLag[lag]...)
		eligible := make([]quotaSegment, 0, len(all))
		for _, segment := range all {
			if common[identityOf(segment)] == 3 {
				eligible = append(eligible, segment)
			}
		}
		sort.Slice(eligible, func(i, j int) bool {
			if eligible[i].EndAt != eligible[j].EndAt {
				return eligible[i].EndAt < eligible[j].EndAt
			}
			a, b := identityOf(eligible[i]), identityOf(eligible[j])
			if a.Account != b.Account {
				return a.Account < b.Account
			}
			if a.Window != b.Window {
				return a.Window < b.Window
			}
			if a.CycleID != b.CycleID {
				return a.CycleID < b.CycleID
			}
			if a.StartEventID != b.StartEventID {
				return a.StartEventID < b.StartEventID
			}
			return a.EndEventID < b.EndEventID
		})
		row := backtestLagResult{Lag: lag, EligibleSegments: len(eligible), FlaggedSegments: len(all) - len(eligible)}
		modes := []string{pricingModeLegacyAPI, pricingModeCurrentAPI, pricingModeCredits, pricingModeLearned}
		accumulators := make(map[string]*scoreAccumulator)
		for _, mode := range modes {
			accumulators[mode] = &scoreAccumulator{}
		}
		if len(eligible) >= 25 {
			for fold := 0; fold < 4; fold++ {
				trainEnd := len(eligible) * (40 + fold*15) / 100
				testEnd := len(eligible) * (55 + fold*15) / 100
				if fold == 3 {
					testEnd = len(eligible)
				}
				if trainEnd < 5 || testEnd <= trainEnd {
					continue
				}
				train := eligible[:trainEnd]
				test := eligible[trainEnd:testEnd]
				cutoff := train[len(train)-1].EndAt
				fit, err := fitQuotaWeights(train, prices, cutoff, opts)
				if err != nil {
					return result, err
				}
				scales := make(map[string]map[string]float64)
				for _, mode := range modes[:3] {
					scales[mode] = fitReferenceScales(train, mode, prices, cutoff, opts.HalfLifeDays)
				}
				for _, segment := range test {
					for _, mode := range modes[:3] {
						scale, ok := scales[mode][modelGroup(segment)]
						if ok {
							accumulators[mode].add(scale*referenceEquivalent(segment, mode, prices), segment.DP)
						}
					}
					if fit.Available && fit.predict(segment, prices) > 0 {
						accumulators[pricingModeLearned].add(fit.predict(segment, prices), segment.DP)
					}
				}
			}
		}
		for _, mode := range modes {
			row.Scores = append(row.Scores, accumulators[mode].result(mode))
		}
		result.Lags = append(result.Lags, row)
		learned := row.Scores[len(row.Scores)-1]
		if learned.SegmentCount > 0 {
			if learned.MAE < bestMAE {
				secondMAE = bestMAE
				bestMAE = learned.MAE
				result.SelectedLag = lag
			} else if learned.MAE < secondMAE {
				secondMAE = learned.MAE
			}
		}
	}
	if !math.IsInf(secondMAE, 1) && bestMAE > 0 {
		result.LagAmbiguous = secondMAE/bestMAE < 1.10
	}
	if len(result.Lags) > result.SelectedLag {
		result.Scores = result.Lags[result.SelectedLag].Scores
	}
	selected := byLag[result.SelectedLag]
	fit, err := fitQuotaWeights(selected, prices, now, opts)
	if err != nil {
		return result, err
	}
	fit.Lag = result.SelectedLag
	result.FittedWeights = fit
	return result, nil
}
