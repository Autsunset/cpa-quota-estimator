package main

import (
	"errors"
	"math"
	"sort"
	"strings"
)

const (
	weightReferenceModel = "gpt-5.6-sol"
	weightObservationSD  = 0.35
	weightHuberDelta     = 0.6
)

type weightLearnerOptions struct {
	HalfLifeDays  float64
	MaxIterations int
}

func defaultWeightLearnerOptions() weightLearnerOptions {
	return weightLearnerOptions{HalfLifeDays: 21, MaxIterations: 80}
}

type weightEstimate struct {
	Value      float64 `json:"value"`
	Low        float64 `json:"low"`
	High       float64 `json:"high"`
	Identified bool    `json:"identified"`
	DataShare  float64 `json:"data_share"`
	Correlated bool    `json:"correlated,omitempty"`
}

type learnedModelWeights struct {
	Model  string         `json:"model"`
	Input  weightEstimate `json:"input"`
	Cache  weightEstimate `json:"cache"`
	Output weightEstimate `json:"output"`
}

type weightFit struct {
	Available      bool                  `json:"available"`
	FittedAt       int64                 `json:"fitted_at"`
	Lag            int                   `json:"lag"`
	SegmentCount   int                   `json:"segment_count"`
	EffectiveCount float64               `json:"effective_count"`
	MeanAbsError   float64               `json:"mean_abs_error"`
	Objective      float64               `json:"objective"`
	Models         []learnedModelWeights `json:"models"`
	Fast           weightEstimate        `json:"fast"`
	LongContext    weightEstimate        `json:"long_context"`
	ParameterNames []string              `json:"parameter_names"`
	LogParameters  []float64             `json:"log_parameters"`
	Covariance     [][]float64           `json:"covariance"`
	HalfLifeDays   float64               `json:"half_life_days"`
}

type weightModel struct {
	names      []string
	index      map[string]int
	priorMean  []float64
	priorSigma []float64
	prices     map[string]price
}

func modelGroup(segment quotaSegment) string { return segment.Account + "|" + segment.Window }

func referencePrice(model, mode string, prices map[string]price) (price, bool) {
	model = normalizeModel(model)
	p, ok := prices[model]
	if !ok {
		if mode == pricingModeCredits {
			return officialCodexCreditPrice(model)
		}
		return price{}, false
	}
	return priceForPricingMode(p, mode), true
}

func referenceFeatureRate(feature segmentFeature, mode string, prices map[string]price) float64 {
	p, ok := referencePrice(feature.Model, mode, prices)
	if !ok {
		return 0
	}
	var rate float64
	switch feature.Type {
	case "input":
		rate = p.Input
		if feature.Long && mode != pricingModeCredits && p.LongInput > 0 {
			rate = p.LongInput
		}
	case "cache":
		rate = p.CacheRead
		if feature.Long && mode != pricingModeCredits && p.LongRead > 0 {
			rate = p.LongRead
		}
	case "output":
		rate = p.Output
		if feature.Long && mode != pricingModeCredits && p.LongOutput > 0 {
			rate = p.LongOutput
		}
	}
	if feature.Fast {
		rate *= 2.5
	}
	return rate
}

func referenceSolRate(mode string, prices map[string]price) float64 {
	p, ok := referencePrice(weightReferenceModel, mode, prices)
	if !ok {
		return 0
	}
	return p.Input
}

func referenceEquivalent(segment quotaSegment, mode string, prices map[string]price) float64 {
	denominator := referenceSolRate(mode, prices)
	if denominator <= 0 {
		return 0
	}
	var total float64
	for _, feature := range segment.Features {
		total += float64(feature.Tokens) / 1_000_000 * referenceFeatureRate(feature, mode, prices) / denominator
	}
	return total
}

func newWeightModel(segments []quotaSegment, prices map[string]price) weightModel {
	groups := make(map[string]bool)
	models := make(map[string]bool)
	for _, segment := range segments {
		groups[modelGroup(segment)] = true
		for _, feature := range segment.Features {
			if normalizeModel(feature.Model) != weightReferenceModel && referenceFeatureRate(feature, pricingModeCredits, prices) > 0 {
				models[normalizeModel(feature.Model)] = true
			}
		}
	}
	// Keep newly introduced target models visible as prior-only estimates until
	// their traffic crosses enough quota boundaries to identify them.
	for _, model := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if referenceFeatureRate(segmentFeature{Model: model, Type: "input"}, pricingModeCredits, prices) > 0 {
			models[model] = true
		}
	}
	var groupNames, modelNames []string
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	for model := range models {
		modelNames = append(modelNames, model)
	}
	sort.Strings(groupNames)
	sort.Strings(modelNames)
	m := weightModel{index: make(map[string]int), prices: prices}
	add := func(name string, mean, sigma float64) {
		m.index[name] = len(m.names)
		m.names = append(m.names, name)
		m.priorMean = append(m.priorMean, mean)
		m.priorSigma = append(m.priorSigma, sigma)
	}
	for _, group := range groupNames {
		var ratios []float64
		for _, segment := range segments {
			if modelGroup(segment) != group {
				continue
			}
			value := referenceEquivalent(segment, pricingModeCredits, prices)
			if value > 0 && segment.DP > 0 {
				ratios = append(ratios, segment.DP/value)
			}
		}
		sort.Float64s(ratios)
		initial := 1.0
		if len(ratios) > 0 {
			initial = ratios[len(ratios)/2]
		}
		add("scale:"+group, math.Log(initial), 2.5)
	}
	for _, model := range modelNames {
		add("model:"+model, 0, .5)
	}
	add("type:cache", 0, .5)
	add("type:output", 0, .5)
	add("fast", math.Log(2.5), .5)
	add("long", 0, .5)
	return m
}

func (m weightModel) predict(segment quotaSegment, x []float64) (float64, []float64) {
	grad := make([]float64, len(x))
	scaleIndex, ok := m.index["scale:"+modelGroup(segment)]
	if !ok {
		return 0, grad
	}
	scale := math.Exp(x[scaleIndex])
	denominator := referenceSolRate(pricingModeCredits, m.prices)
	if denominator <= 0 {
		return 0, grad
	}
	var value float64
	for _, feature := range segment.Features {
		rate := referenceFeatureRate(feature, pricingModeCredits, m.prices)
		if rate <= 0 {
			continue
		}
		term := float64(feature.Tokens) / 1_000_000 * rate / denominator
		modelIndex, hasModel := m.index["model:"+normalizeModel(feature.Model)]
		if hasModel {
			term *= math.Exp(x[modelIndex])
		}
		typeIndex, hasType := m.index["type:"+feature.Type]
		if hasType {
			term *= math.Exp(x[typeIndex])
		}
		fastIndex := m.index["fast"]
		if feature.Fast {
			// The published credit reference already applies 2.5x Fast. The
			// learned Fast parameter replaces that multiplier.
			term *= math.Exp(x[fastIndex] - math.Log(2.5))
		}
		longIndex := m.index["long"]
		if feature.Long {
			term *= math.Exp(x[longIndex])
		}
		value += term
		weighted := scale * term
		if hasModel {
			grad[modelIndex] += weighted
		}
		if hasType {
			grad[typeIndex] += weighted
		}
		if feature.Fast {
			grad[fastIndex] += weighted
		}
		if feature.Long {
			grad[longIndex] += weighted
		}
	}
	prediction := scale * value
	grad[scaleIndex] = prediction
	return prediction, grad
}

func segmentFitWeight(segment quotaSegment, now int64, halfLifeDays float64) float64 {
	if halfLifeDays <= 0 {
		halfLifeDays = 21
	}
	ageDays := math.Max(0, float64(now-segment.EndAt)/86400)
	return math.Max(.05, segment.BoundaryWeight) * math.Exp(-math.Ln2*ageDays/halfLifeDays)
}

func huberLoss(residual, delta float64) float64 {
	a := math.Abs(residual)
	if a <= delta {
		return .5 * a * a
	}
	return delta * (a - .5*delta)
}

func weightObjective(m weightModel, segments []quotaSegment, x []float64, now int64, opts weightLearnerOptions) float64 {
	var total float64
	for _, segment := range segments {
		prediction, _ := m.predict(segment, x)
		residual := segment.DP - prediction
		total += segmentFitWeight(segment, now, opts.HalfLifeDays) * huberLoss(residual, weightHuberDelta) / (weightObservationSD * weightObservationSD)
	}
	for i := range x {
		d := (x[i] - m.priorMean[i]) / m.priorSigma[i]
		total += .5 * d * d
	}
	return total
}

func weightNormalEquations(m weightModel, segments []quotaSegment, x []float64, now int64, opts weightLearnerOptions) ([][]float64, []float64, float64) {
	n := len(x)
	h := make([][]float64, n)
	for i := range h {
		h[i] = make([]float64, n)
	}
	b := make([]float64, n)
	var effective float64
	for _, segment := range segments {
		prediction, grad := m.predict(segment, x)
		residual := segment.DP - prediction
		weight := segmentFitWeight(segment, now, opts.HalfLifeDays)
		effective += weight
		if a := math.Abs(residual); a > weightHuberDelta {
			weight *= weightHuberDelta / a
		}
		weight /= weightObservationSD * weightObservationSD
		for i := 0; i < n; i++ {
			b[i] += weight * grad[i] * residual
			for j := 0; j <= i; j++ {
				h[i][j] += weight * grad[i] * grad[j]
			}
		}
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			h[i][j] = h[j][i]
		}
		precision := 1 / (m.priorSigma[i] * m.priorSigma[i])
		h[i][i] += precision
		b[i] -= precision * (x[i] - m.priorMean[i])
	}
	return h, b, effective
}

func solveDense(matrix [][]float64, rhs []float64) ([]float64, bool) {
	n := len(rhs)
	a := make([][]float64, n)
	b := append([]float64(nil), rhs...)
	for i := range a {
		a[i] = append([]float64(nil), matrix[i]...)
	}
	for col := 0; col < n; col++ {
		pivot := col
		for row := col + 1; row < n; row++ {
			if math.Abs(a[row][col]) > math.Abs(a[pivot][col]) {
				pivot = row
			}
		}
		if math.Abs(a[pivot][col]) < 1e-12 {
			return nil, false
		}
		a[col], a[pivot] = a[pivot], a[col]
		b[col], b[pivot] = b[pivot], b[col]
		factor := a[col][col]
		for j := col; j < n; j++ {
			a[col][j] /= factor
		}
		b[col] /= factor
		for row := 0; row < n; row++ {
			if row == col {
				continue
			}
			factor = a[row][col]
			for j := col; j < n; j++ {
				a[row][j] -= factor * a[col][j]
			}
			b[row] -= factor * b[col]
		}
	}
	return b, true
}

func invertDense(matrix [][]float64) ([][]float64, bool) {
	n := len(matrix)
	inverse := make([][]float64, n)
	for i := range inverse {
		inverse[i] = make([]float64, n)
	}
	for col := 0; col < n; col++ {
		rhs := make([]float64, n)
		rhs[col] = 1
		solution, ok := solveDense(matrix, rhs)
		if !ok {
			return nil, false
		}
		for row := 0; row < n; row++ {
			inverse[row][col] = solution[row]
		}
	}
	return inverse, true
}

func fitQuotaWeights(all []quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions) (weightFit, error) {
	segments := make([]quotaSegment, 0, len(all))
	for _, segment := range all {
		if segment.eligible() && segment.DP > 0 && referenceEquivalent(segment, pricingModeCredits, prices) > 0 {
			segments = append(segments, segment)
		}
	}
	fit := weightFit{FittedAt: now, SegmentCount: len(segments), HalfLifeDays: opts.HalfLifeDays}
	if len(segments) < 5 {
		return fit, nil
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 80
	}
	m := newWeightModel(segments, prices)
	if len(m.names) == 0 || len(m.names) > 24 {
		return fit, errors.New("weight model has unsupported parameter count")
	}
	x := append([]float64(nil), m.priorMean...)
	damping := .01
	objective := weightObjective(m, segments, x, now, opts)
	for iteration := 0; iteration < opts.MaxIterations; iteration++ {
		h, b, _ := weightNormalEquations(m, segments, x, now, opts)
		for i := range h {
			h[i][i] += damping * (1 + h[i][i])
		}
		step, ok := solveDense(h, b)
		if !ok {
			return fit, errors.New("weight LM normal equations are singular")
		}
		candidate := append([]float64(nil), x...)
		var stepNorm float64
		for i := range candidate {
			candidate[i] = math.Max(-20, math.Min(20, candidate[i]+step[i]))
			stepNorm += step[i] * step[i]
		}
		newObjective := weightObjective(m, segments, candidate, now, opts)
		if newObjective < objective {
			x, objective = candidate, newObjective
			damping = math.Max(1e-8, damping/2)
			if stepNorm < 1e-12 {
				break
			}
		} else {
			damping *= 4
			if damping > 1e12 {
				break
			}
		}
	}
	h, _, effective := weightNormalEquations(m, segments, x, now, opts)
	covariance, ok := invertDense(h)
	if !ok {
		return fit, errors.New("weight posterior Hessian is singular")
	}
	fit.Available = true
	fit.EffectiveCount = effective
	fit.Objective = objective
	fit.ParameterNames = m.names
	fit.LogParameters = x
	fit.Covariance = covariance
	fit.Lag = segments[0].Lag
	var absoluteError float64
	for _, segment := range segments {
		prediction, _ := m.predict(segment, x)
		absoluteError += math.Abs(segment.DP - prediction)
	}
	fit.MeanAbsError = absoluteError / float64(len(segments))
	fit.Models = deriveModelWeights(m, x, covariance)
	fit.Fast = deriveFactorEstimate(m, x, covariance, []string{"fast"}, 1)
	fit.LongContext = deriveFactorEstimate(m, x, covariance, []string{"long"}, 1)
	return fit, nil
}

func deriveFactorEstimate(m weightModel, x []float64, covariance [][]float64, names []string, base float64) weightEstimate {
	var logValue, variance, priorVariance float64
	correlated := false
	for _, name := range names {
		if i, ok := m.index[name]; ok {
			logValue += x[i]
			priorVariance += m.priorSigma[i] * m.priorSigma[i]
			for _, other := range names {
				if j, exists := m.index[other]; exists {
					variance += covariance[i][j]
				}
			}
			for j := range covariance {
				if j == i || covariance[i][i] <= 0 || covariance[j][j] <= 0 {
					continue
				}
				correlation := covariance[i][j] / math.Sqrt(covariance[i][i]*covariance[j][j])
				if math.Abs(correlation) >= .9 {
					correlated = true
				}
			}
		}
	}
	variance = math.Max(0, variance)
	std := math.Sqrt(variance)
	share := 1.0
	if priorVariance > 0 {
		share = math.Max(0, math.Min(1, 1-variance/priorVariance))
	}
	return weightEstimate{
		Value: base * math.Exp(logValue), Low: base * math.Exp(logValue-1.96*std),
		High: base * math.Exp(logValue+1.96*std), Identified: share >= .5 && !correlated, DataShare: share, Correlated: correlated,
	}
}

func deriveModelWeights(m weightModel, x []float64, covariance [][]float64) []learnedModelWeights {
	models := map[string]bool{weightReferenceModel: true}
	for _, name := range m.names {
		if strings.HasPrefix(name, "model:") {
			models[strings.TrimPrefix(name, "model:")] = true
		}
	}
	var names []string
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	denominator := referenceSolRate(pricingModeCredits, m.prices)
	result := make([]learnedModelWeights, 0, len(names))
	for _, model := range names {
		row := learnedModelWeights{Model: model}
		for _, typ := range []string{"input", "cache", "output"} {
			base := referenceFeatureRate(segmentFeature{Model: model, Type: typ}, pricingModeCredits, m.prices) / denominator
			if base <= 0 {
				continue
			}
			parameterNames := []string{}
			if model != weightReferenceModel {
				parameterNames = append(parameterNames, "model:"+model)
			}
			if typ != "input" {
				parameterNames = append(parameterNames, "type:"+typ)
			}
			estimate := deriveFactorEstimate(m, x, covariance, parameterNames, base)
			switch typ {
			case "input":
				row.Input = estimate
			case "cache":
				row.Cache = estimate
			case "output":
				row.Output = estimate
			}
		}
		result = append(result, row)
	}
	return result
}

func (fit weightFit) predict(segment quotaSegment, prices map[string]price) float64 {
	if !fit.Available {
		return 0
	}
	m := weightModel{index: make(map[string]int), names: fit.ParameterNames, prices: prices}
	for i, name := range fit.ParameterNames {
		m.index[name] = i
	}
	prediction, _ := m.predict(segment, fit.LogParameters)
	return prediction
}
