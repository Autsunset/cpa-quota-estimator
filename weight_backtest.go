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

type interruptionSensitivity struct {
	EligibleSegments         int            `json:"eligible_segments"`
	TestSegments             int            `json:"test_segments"`
	InterruptedTestSegments  int            `json:"interrupted_test_segments"`
	WithoutFeature           backtestScore  `json:"without_feature"`
	WithFeature              backtestScore  `json:"with_feature"`
	PctPerInterruptedRequest weightEstimate `json:"pct_per_interrupted_request"`
}

type scoreAccumulator struct {
	absError float64
	bias     float64
	count    int
}

type segmentIdentity struct {
	Account       string
	Window        string
	CycleID       int64
	RegimeResetAt int64
	StartEventID  int64
	EndEventID    int64
}

func identityOf(segment quotaSegment) segmentIdentity {
	return segmentIdentity{segment.Account, segment.Window, segment.CycleID, segment.RegimeResetAt, segment.StartEventID, segment.EndEventID}
}

func sortSegmentsChronologically(segments []quotaSegment) {
	sort.Slice(segments, func(i, j int) bool {
		a, b := segments[i], segments[j]
		if a.EndAt != b.EndAt {
			return a.EndAt < b.EndAt
		}
		ia, ib := identityOf(a), identityOf(b)
		if ia.Account != ib.Account {
			return ia.Account < ib.Account
		}
		if ia.Window != ib.Window {
			return ia.Window < ib.Window
		}
		if ia.CycleID != ib.CycleID {
			return ia.CycleID < ib.CycleID
		}
		if ia.RegimeResetAt != ib.RegimeResetAt {
			return ia.RegimeResetAt < ib.RegimeResetAt
		}
		if ia.StartEventID != ib.StartEventID {
			return ia.StartEventID < ib.StartEventID
		}
		return ia.EndEventID < ib.EndEventID
	})
}

func runInterruptedSensitivity(all []quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions) (interruptionSensitivity, error) {
	segments := make([]quotaSegment, 0, len(all))
	for _, segment := range all {
		if segment.eligibleWithInterrupted() && segment.DP > 0 && referenceEquivalent(segment, pricingModeCredits, prices) > 0 {
			segments = append(segments, segment)
		}
	}
	sortSegmentsChronologically(segments)
	result := interruptionSensitivity{EligibleSegments: len(segments)}
	if len(segments) < 25 {
		return result, nil
	}
	start := len(segments) * 40 / 100
	if start < 25 {
		start = 25
	}
	result.TestSegments = len(segments) - start
	for _, segment := range segments[start:] {
		if segment.InterruptedCount > 0 {
			result.InterruptedTestSegments++
		}
	}
	without := weightOptionsWithDefaults(opts)
	without.IncludeInterrupted = true
	without.FitInterruptedCoefficient = false
	scores, err := prequentialScores(segments, prices, without)
	if err != nil {
		return result, err
	}
	result.WithoutFeature = scores[len(scores)-1]
	with := without
	with.FitInterruptedCoefficient = true
	scores, err = prequentialScores(segments, prices, with)
	if err != nil {
		return result, err
	}
	result.WithFeature = scores[len(scores)-1]
	fit, err := fitQuotaWeights(segments, prices, now, with)
	if err != nil {
		return result, err
	}
	result.PctPerInterruptedRequest = fit.Interrupted
	return result, nil
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

func prequentialScores(eligible []quotaSegment, prices map[string]price, opts weightLearnerOptions) ([]backtestScore, error) {
	const modelDiagnostic = "weights_model"
	modes := []string{pricingModeAPI, pricingModeCredits, modelDiagnostic}
	accumulators := make(map[string]*scoreAccumulator)
	for _, mode := range modes {
		accumulators[mode] = &scoreAccumulator{}
	}
	if len(eligible) < 25 {
		var empty []backtestScore
		for _, mode := range modes {
			empty = append(empty, accumulators[mode].result(mode))
		}
		return empty, nil
	}
	start := len(eligible) * 40 / 100
	if start < 25 {
		start = 25
	}
	training := eligible[:start]
	cutoff := training[len(training)-1].EndAt
	learnedFit, err := fitQuotaWeights(training, prices, cutoff, opts)
	if err != nil {
		return nil, err
	}
	learnedTracker := newOnlineScaleTracker(learnedFit, opts.RandomWalkSigma)
	trackers := make(map[string]*onlineScaleTracker)
	for _, mode := range modes[:2] {
		fit, fitErr := fitReferenceCycleScales(training, prices, mode, cutoff, opts)
		if fitErr != nil {
			return nil, fitErr
		}
		trackers[mode] = newOnlineScaleTracker(fit, opts.RandomWalkSigma)
	}
	for index := start; index < len(eligible); index++ {
		// Shared model factors are refreshed at a fixed cadence; every cycle
		// scale is updated after each observed crossing below.
		if index > start && (index-start)%25 == 0 {
			learnedFit, err = fitQuotaWeights(eligible[:index], prices, eligible[index-1].EndAt, opts)
			if err != nil {
				return nil, err
			}
			learnedTracker = newOnlineScaleTracker(learnedFit, opts.RandomWalkSigma)
		}
		segment := eligible[index]
		weight := segmentFitWeight(segment, segment.EndAt, opts.HalfLifeDays)
		for _, mode := range modes[:2] {
			equivalent := referenceEquivalent(segment, mode, prices)
			if equivalent <= 0 {
				continue
			}
			state := trackers[mode].forSegment(segment)
			prediction := math.Exp(state.LogValue) * equivalent
			accumulators[mode].add(prediction, segment.DP)
			state.update(equivalent, segment.DP, weight, 0)
		}
		if learnedFit.Available {
			equivalent := learnedSegmentEquivalent(segment, learnedFit)
			if equivalent > 0 {
				state := learnedTracker.forSegment(segment)
				interrupt := 0.0
				if opts.IncludeInterrupted && opts.FitInterruptedCoefficient {
					interrupt = learnedFit.Interrupted.Value * float64(segment.InterruptedCount)
				}
				prediction := math.Exp(state.LogValue)*equivalent + interrupt
				accumulators[modelDiagnostic].add(prediction, segment.DP)
				state.update(equivalent, segment.DP, weight, interrupt)
			}
		}
	}
	var scores []backtestScore
	for _, mode := range modes {
		scores = append(scores, accumulators[mode].result(mode))
	}
	return scores, nil
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
		sortSegmentsChronologically(eligible)
		row := backtestLagResult{Lag: lag, EligibleSegments: len(eligible), FlaggedSegments: len(all) - len(eligible)}
		var err error
		row.Scores, err = prequentialScores(eligible, prices, opts)
		if err != nil {
			return result, err
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
