package main

import (
	"errors"
	"math"
	"sort"
	"strconv"
)

func newReferenceWeightModel(segments []quotaSegment, prices map[string]price, mode string, opts weightLearnerOptions) weightModel {
	opts = weightOptionsWithDefaults(opts)
	cycles := collectLearningCycles(segments)
	m := weightModel{index: make(map[string]int), prices: prices, referenceMode: mode, cycles: cycles}
	previousGroup := ""
	previousIndex := -1
	for _, cycle := range cycles {
		var ratios []float64
		for _, segment := range segments {
			if modelCycle(segment) != cycle.Key {
				continue
			}
			value := referenceEquivalent(segment, mode, prices)
			if value > 0 && segment.DP > 0 {
				ratios = append(ratios, segment.DP/value)
			}
		}
		sort.Float64s(ratios)
		initial := 1.0
		if len(ratios) > 0 {
			initial = ratios[len(ratios)/2]
		}
		sigma := 2.5
		if cycle.Group == previousGroup && previousIndex >= 0 {
			sigma = 1e6
		}
		name := "scale:" + cycle.Key
		currentIndex := len(m.names)
		m.index[name] = currentIndex
		m.names = append(m.names, name)
		m.priorMean = append(m.priorMean, math.Log(initial))
		m.priorSigma = append(m.priorSigma, sigma)
		if cycle.Group == previousGroup && previousIndex >= 0 {
			m.randomWalk = append(m.randomWalk, randomWalkEdge{Previous: previousIndex, Current: currentIndex, Sigma: opts.RandomWalkSigma})
		}
		previousGroup, previousIndex = cycle.Group, currentIndex
	}
	return m
}

func fitReferenceCycleScales(segments []quotaSegment, prices map[string]price, mode string, now int64, opts weightLearnerOptions) (weightFit, error) {
	opts = weightOptionsWithDefaults(opts)
	fit := weightFit{FittedAt: now, SegmentCount: len(segments), RandomWalkSigma: opts.RandomWalkSigma}
	if len(segments) < 5 {
		return fit, nil
	}
	m := newReferenceWeightModel(segments, prices, mode, opts)
	if len(m.names) == 0 || len(m.names) > 64 {
		return fit, errors.New("reference scale model has unsupported cycle count")
	}
	x, cov, effective, objective, err := optimizeWeightModel(m, segments, now, opts)
	if err != nil {
		return fit, err
	}
	fit.Available = true
	fit.EffectiveCount = effective
	fit.Objective = objective
	fit.ParameterNames = m.names
	fit.LogParameters = x
	fit.Covariance = cov
	fit.CycleScales = deriveCycleScales(m, x, cov)
	return fit, nil
}

type onlineCycleScale struct {
	LogValue  float64
	Precision float64
}

type onlineScaleTracker struct {
	ByCycle         map[string]*onlineCycleScale
	LastByGroup     map[string]*onlineCycleScale
	RandomWalkSigma float64
}

func newOnlineScaleTracker(fit weightFit, sigma float64) *onlineScaleTracker {
	tracker := &onlineScaleTracker{ByCycle: make(map[string]*onlineCycleScale), LastByGroup: make(map[string]*onlineCycleScale), RandomWalkSigma: sigma}
	for _, cycle := range fit.CycleScales {
		if cycle.Scale.Value <= 0 {
			continue
		}
		std := (math.Log(cycle.Scale.High) - math.Log(cycle.Scale.Low)) / (2 * 1.96)
		precision := .001
		if std > 0 {
			precision = 1 / (std * std)
		}
		state := &onlineCycleScale{LogValue: math.Log(cycle.Scale.Value), Precision: precision}
		key := cycle.Account + "|" + cycle.Window + "|" + strconv.FormatInt(cycle.CycleID, 10) + "|" + strconv.FormatInt(cycle.RegimeResetAt, 10)
		tracker.ByCycle[key] = state
		tracker.LastByGroup[cycle.Account+"|"+cycle.Window] = state
	}
	return tracker
}

func (t *onlineScaleTracker) forSegment(segment quotaSegment) *onlineCycleScale {
	key := modelCycle(segment)
	if state := t.ByCycle[key]; state != nil {
		return state
	}
	prior := t.LastByGroup[modelGroup(segment)]
	logValue := 0.0
	if prior != nil {
		logValue = prior.LogValue
	}
	sigma := t.RandomWalkSigma
	if sigma <= 0 {
		sigma = .35
	}
	precision := 1 / (sigma * sigma)
	if prior == nil {
		precision = 1 / (2.5 * 2.5)
	}
	state := &onlineCycleScale{LogValue: logValue, Precision: precision}
	t.ByCycle[key] = state
	t.LastByGroup[modelGroup(segment)] = state
	return state
}

func (s *onlineCycleScale) update(equivalent, observed, weight, interruptedContribution float64) {
	if equivalent <= 0 {
		return
	}
	prior := s.LogValue
	precision := s.Precision
	x := prior
	for iteration := 0; iteration < 12; iteration++ {
		base := math.Exp(x) * equivalent
		residual := observed - interruptedContribution - base
		robust := weight / (weightObservationSD * weightObservationSD)
		if a := math.Abs(residual); a > weightHuberDelta {
			robust *= weightHuberDelta / a
		}
		curvature := precision + robust*base*base
		step := (robust*base*residual - precision*(x-prior)) / curvature
		step = math.Max(-1, math.Min(1, step))
		x += step
		if math.Abs(step) < 1e-7 {
			break
		}
	}
	base := math.Exp(x) * equivalent
	residual := observed - interruptedContribution - base
	robust := weight / (weightObservationSD * weightObservationSD)
	if a := math.Abs(residual); a > weightHuberDelta {
		robust *= weightHuberDelta / a
	}
	s.LogValue = x
	s.Precision = precision + robust*base*base
}

func learnedSegmentEquivalent(segment quotaSegment, fit weightFit) float64 {
	if !fit.Available {
		return 0
	}
	var value float64
	for _, feature := range segment.Features {
		var row *learnedModelWeights
		for i := range fit.Models {
			if fit.Models[i].Model == normalizeModel(feature.Model) {
				row = &fit.Models[i]
				break
			}
		}
		if row == nil {
			continue
		}
		rate := 0.0
		switch feature.Type {
		case "input":
			rate = row.Input.Value
		case "cache":
			rate = row.Cache.Value
		case "output":
			rate = row.Output.Value
		}
		if feature.Fast {
			rate *= fit.Fast.Value
		}
		if feature.Long {
			rate *= fit.LongContext.Value
		}
		value += float64(feature.Tokens) / 1_000_000 * rate
	}
	return value
}
