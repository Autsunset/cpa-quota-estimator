package main

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	weightReferenceModel = "gpt-5.6-sol"
	weightObservationSD  = 0.35
	weightHuberDelta     = 0.6
)

type weightLearnerOptions struct {
	HalfLifeDays              float64
	MaxIterations             int
	RandomWalkSigma           float64
	CacheShareMinSD           float64
	OutputShareMinSD          float64
	TypeMinFisher             float64
	ModelShareMinSD           float64
	ModelMinFisher            float64
	IncludeInterrupted        bool
	FitInterruptedCoefficient bool
	FixedModelFactors         map[string]float64
	CacheCycleRatioMinRange   float64
	OutputCycleRatioMinRange  float64
}

func defaultWeightLearnerOptions() weightLearnerOptions {
	return weightLearnerOptions{HalfLifeDays: 21, MaxIterations: 80, RandomWalkSigma: .35,
		CacheShareMinSD: .08, OutputShareMinSD: .02, TypeMinFisher: 20,
		ModelShareMinSD: .05, ModelMinFisher: 10,
		CacheCycleRatioMinRange: .10, OutputCycleRatioMinRange: .02}
}

type identifiabilityDiagnostic struct {
	Name               string  `json:"name"`
	Unlocked           bool    `json:"unlocked"`
	WithinCycleShareSD float64 `json:"within_cycle_share_sd"`
	ConditionalFisher  float64 `json:"conditional_fisher"`
	MinShareSD         float64 `json:"min_share_sd"`
	MinFisher          float64 `json:"min_fisher"`
	CycleRatioRange    float64 `json:"cycle_ratio_range,omitempty"`
	MinCycleRatioRange float64 `json:"min_cycle_ratio_range,omitempty"`
	CyclesCompared     int     `json:"cycles_compared,omitempty"`
}

type learnedCycleScale struct {
	Account           string         `json:"account"`
	Window            string         `json:"window"`
	CycleID           int64          `json:"cycle_id"`
	RegimeResetAt     int64          `json:"regime_reset_at"`
	StartedAt         int64          `json:"started_at"`
	Scale             weightEstimate `json:"scale"`
	CreditsPerPercent weightEstimate `json:"credits_per_percent"`
}

type weightEstimate struct {
	Value       float64 `json:"value"`
	Low         float64 `json:"low"`
	High        float64 `json:"high"`
	Identified  bool    `json:"identified"`
	DataShare   float64 `json:"data_share"`
	Correlated  bool    `json:"correlated,omitempty"`
	PriorLocked bool    `json:"prior_locked,omitempty"`
}

type learnedModelWeights struct {
	Model  string         `json:"model"`
	Input  weightEstimate `json:"input"`
	Cache  weightEstimate `json:"cache"`
	Output weightEstimate `json:"output"`
}

type weightFit struct {
	Available        bool                        `json:"available"`
	FittedAt         int64                       `json:"fitted_at"`
	Lag              int                         `json:"lag"`
	SegmentCount     int                         `json:"segment_count"`
	EffectiveCount   float64                     `json:"effective_count"`
	MeanAbsError     float64                     `json:"mean_abs_error"`
	Objective        float64                     `json:"objective"`
	Models           []learnedModelWeights       `json:"models"`
	Fast             weightEstimate              `json:"fast"`
	LongContext      weightEstimate              `json:"long_context"`
	ParameterNames   []string                    `json:"parameter_names"`
	LogParameters    []float64                   `json:"log_parameters"`
	Covariance       [][]float64                 `json:"covariance"`
	HalfLifeDays     float64                     `json:"half_life_days"`
	RandomWalkSigma  float64                     `json:"random_walk_sigma"`
	TypeDiagnostics  []identifiabilityDiagnostic `json:"type_diagnostics"`
	ModelDiagnostics []identifiabilityDiagnostic `json:"model_diagnostics"`
	CycleScales      []learnedCycleScale         `json:"cycle_scales"`
	MaxEndEventID    int64                       `json:"max_end_event_id"`
	Interrupted      weightEstimate              `json:"interrupted,omitempty"`
}

type randomWalkEdge struct {
	Previous, Current int
	Sigma             float64
}

type learningCycle struct {
	Key           string
	Group         string
	Account       string
	Window        string
	CycleID       int64
	RegimeResetAt int64
	StartedAt     int64
}

type weightModel struct {
	names              []string
	index              map[string]int
	priorMean          []float64
	priorSigma         []float64
	prices             map[string]price
	randomWalk         []randomWalkEdge
	cycles             []learningCycle
	typeDiagnostics    []identifiabilityDiagnostic
	modelDiagnostics   []identifiabilityDiagnostic
	includeInterrupted bool
	referenceMode      string
	fixedModels        map[string]float64
}

func modelGroup(segment quotaSegment) string { return segment.Account + "|" + segment.Window }

func modelCycle(segment quotaSegment) string {
	return modelGroup(segment) + "|" + strconv.FormatInt(segment.CycleID, 10) + "|" + strconv.FormatInt(segment.RegimeResetAt, 10)
}

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

func collectLearningCycles(segments []quotaSegment) []learningCycle {
	cyclesByKey := make(map[string]learningCycle)
	for _, segment := range segments {
		key := modelCycle(segment)
		cycle, ok := cyclesByKey[key]
		if !ok || segment.StartAt < cycle.StartedAt {
			cyclesByKey[key] = learningCycle{Key: key, Group: modelGroup(segment), Account: segment.Account,
				Window: segment.Window, CycleID: segment.CycleID, RegimeResetAt: segment.RegimeResetAt, StartedAt: segment.StartAt}
		}
	}
	cycles := make([]learningCycle, 0, len(cyclesByKey))
	for _, cycle := range cyclesByKey {
		cycles = append(cycles, cycle)
	}
	sort.Slice(cycles, func(i, j int) bool {
		if cycles[i].Group != cycles[j].Group {
			return cycles[i].Group < cycles[j].Group
		}
		if cycles[i].StartedAt != cycles[j].StartedAt {
			return cycles[i].StartedAt < cycles[j].StartedAt
		}
		return cycles[i].Key < cycles[j].Key
	})
	return cycles
}

func newWeightModel(segments []quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions) weightModel {
	opts = weightOptionsWithDefaults(opts)
	cycles := collectLearningCycles(segments)
	typeDiagnostics, modelDiagnostics := assessWeightIdentifiability(segments, prices, now, opts)
	m := weightModel{index: make(map[string]int), prices: prices, cycles: cycles, referenceMode: pricingModeCredits, fixedModels: opts.FixedModelFactors,
		typeDiagnostics: typeDiagnostics, modelDiagnostics: modelDiagnostics, includeInterrupted: opts.IncludeInterrupted}
	add := func(name string, mean, sigma float64) {
		m.index[name] = len(m.names)
		m.names = append(m.names, name)
		m.priorMean = append(m.priorMean, mean)
		m.priorSigma = append(m.priorSigma, sigma)
	}
	var previousGroup string
	previousIndex := -1
	for _, cycle := range cycles {
		var ratios []float64
		for _, segment := range segments {
			if modelCycle(segment) != cycle.Key {
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
		sigma := 2.5
		if cycle.Group == previousGroup && previousIndex >= 0 {
			sigma = 1e6
		}
		add("scale:"+cycle.Key, math.Log(initial), sigma)
		currentIndex := m.index["scale:"+cycle.Key]
		if cycle.Group == previousGroup && previousIndex >= 0 {
			m.randomWalk = append(m.randomWalk, randomWalkEdge{Previous: previousIndex, Current: currentIndex, Sigma: opts.RandomWalkSigma})
		}
		previousGroup, previousIndex = cycle.Group, currentIndex
	}
	for _, diagnostic := range modelDiagnostics {
		if diagnostic.Unlocked && opts.FixedModelFactors[strings.TrimPrefix(diagnostic.Name, "model:")] <= 0 {
			add(diagnostic.Name, 0, .5)
		}
	}
	for _, diagnostic := range typeDiagnostics {
		if diagnostic.Unlocked {
			add(diagnostic.Name, 0, .5)
		}
	}
	add("fast", math.Log(2.5), .5)
	add("long", 0, .5)
	if opts.IncludeInterrupted && opts.FitInterruptedCoefficient {
		add("interrupt", math.Log(.02), 1.5)
	}
	return m
}

func (m weightModel) predict(segment quotaSegment, x []float64) (float64, []float64) {
	grad := make([]float64, len(x))
	scaleIndex, ok := m.index["scale:"+modelCycle(segment)]
	if !ok {
		prefix := "scale:" + modelGroup(segment) + "|"
		for i, name := range m.names {
			if strings.HasPrefix(name, prefix) {
				scaleIndex, ok = i, true
			}
		}
	}
	if !ok {
		return 0, grad
	}
	scale := math.Exp(x[scaleIndex])
	mode := m.referenceMode
	if mode == "" {
		mode = pricingModeCredits
	}
	denominator := referenceSolRate(mode, m.prices)
	if denominator <= 0 {
		return 0, grad
	}
	var value float64
	for _, feature := range segment.Features {
		rate := referenceFeatureRate(feature, mode, m.prices)
		if rate <= 0 {
			continue
		}
		term := float64(feature.Tokens) / 1_000_000 * rate / denominator
		modelIndex, hasModel := m.index["model:"+normalizeModel(feature.Model)]
		if hasModel {
			term *= math.Exp(x[modelIndex])
		} else if fixed := m.fixedModels[normalizeModel(feature.Model)]; fixed > 0 {
			term *= fixed
		}
		typeIndex, hasType := m.index["type:"+feature.Type]
		if hasType {
			term *= math.Exp(x[typeIndex])
		}
		fastIndex, hasFast := m.index["fast"]
		if feature.Fast && hasFast {
			// The published credit reference already applies 2.5x Fast. The
			// learned Fast parameter replaces that multiplier.
			term *= math.Exp(x[fastIndex] - math.Log(2.5))
		}
		longIndex, hasLong := m.index["long"]
		if feature.Long && hasLong {
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
		if feature.Fast && hasFast {
			grad[fastIndex] += weighted
		}
		if feature.Long && hasLong {
			grad[longIndex] += weighted
		}
	}
	prediction := scale * value
	grad[scaleIndex] = prediction
	if interruptIndex, exists := m.index["interrupt"]; exists && segment.InterruptedCount > 0 {
		contribution := math.Exp(x[interruptIndex]) * float64(segment.InterruptedCount)
		prediction += contribution
		grad[interruptIndex] = contribution
	}
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
	for _, edge := range m.randomWalk {
		d := (x[edge.Current] - x[edge.Previous]) / edge.Sigma
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
	for _, edge := range m.randomWalk {
		precision := 1 / (edge.Sigma * edge.Sigma)
		delta := x[edge.Current] - x[edge.Previous]
		h[edge.Current][edge.Current] += precision
		h[edge.Previous][edge.Previous] += precision
		h[edge.Current][edge.Previous] -= precision
		h[edge.Previous][edge.Current] -= precision
		b[edge.Current] -= precision * delta
		b[edge.Previous] += precision * delta
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

func optimizeWeightModel(m weightModel, segments []quotaSegment, now int64, opts weightLearnerOptions) ([]float64, [][]float64, float64, float64, error) {
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
			return nil, nil, 0, 0, errors.New("weight LM normal equations are singular")
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
		return nil, nil, 0, 0, errors.New("weight posterior Hessian is singular")
	}
	return x, covariance, effective, objective, nil
}

func fitQuotaWeights(all []quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions) (weightFit, error) {
	opts = weightOptionsWithDefaults(opts)
	segments := make([]quotaSegment, 0, len(all))
	for _, segment := range all {
		eligible := segment.eligible()
		if opts.IncludeInterrupted {
			eligible = segment.eligibleWithInterrupted()
		}
		if eligible && segment.DP > 0 && referenceEquivalent(segment, pricingModeCredits, prices) > 0 {
			segments = append(segments, segment)
		}
	}
	fit := weightFit{FittedAt: now, SegmentCount: len(segments), HalfLifeDays: opts.HalfLifeDays}
	for _, segment := range segments {
		if segment.EndEventID > fit.MaxEndEventID {
			fit.MaxEndEventID = segment.EndEventID
		}
	}
	if len(segments) < 5 {
		return fit, nil
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 80
	}
	m := newWeightModel(segments, prices, now, opts)
	if len(m.names) == 0 || len(m.names) > 64 {
		return fit, errors.New("weight model has unsupported parameter count")
	}
	// Estimate shared factors with cycle scales effectively free. Cross-cycle
	// capacity drift must not turn a changing model mix into a model multiplier.
	factorModel := m
	factorModel.randomWalk = nil
	factorX, factorCov, _, _, err := optimizeWeightModel(factorModel, segments, now, opts)
	if err != nil {
		return fit, err
	}
	// With shared factors held at their within-cycle estimates, smooth only
	// the cycle scales using the configured random-walk prior.
	scaleModel := m
	scaleModel.priorMean = append([]float64(nil), m.priorMean...)
	scaleModel.priorSigma = append([]float64(nil), m.priorSigma...)
	for i, name := range m.names {
		if !strings.HasPrefix(name, "scale:") {
			scaleModel.priorMean[i] = factorX[i]
			scaleModel.priorSigma[i] = 1e-4
		}
	}
	x, scaleCov, effective, objective, err := optimizeWeightModel(scaleModel, segments, now, opts)
	if err != nil {
		return fit, err
	}
	for i, name := range m.names {
		if !strings.HasPrefix(name, "scale:") {
			x[i] = factorX[i]
		}
	}
	fit.Available = true
	fit.EffectiveCount = effective
	fit.Objective = objective
	fit.ParameterNames = m.names
	fit.LogParameters = x
	fit.Covariance = factorCov
	fit.Lag = segments[0].Lag
	fit.RandomWalkSigma = opts.RandomWalkSigma
	fit.TypeDiagnostics = m.typeDiagnostics
	fit.ModelDiagnostics = m.modelDiagnostics
	var absoluteError float64
	for _, segment := range segments {
		prediction, _ := m.predict(segment, x)
		absoluteError += math.Abs(segment.DP - prediction)
	}
	fit.MeanAbsError = absoluteError / float64(len(segments))
	fit.Models = deriveModelWeights(factorModel, x, factorCov)
	fit.Fast = deriveFactorEstimate(factorModel, x, factorCov, []string{"fast"}, 1)
	fit.LongContext = deriveFactorEstimate(factorModel, x, factorCov, []string{"long"}, 1)
	if opts.IncludeInterrupted && opts.FitInterruptedCoefficient {
		fit.Interrupted = deriveFactorEstimate(factorModel, x, factorCov, []string{"interrupt"}, 1)
	}
	fit.CycleScales = deriveCycleScales(scaleModel, x, scaleCov)
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
	modelUnlocked := make(map[string]bool)
	typeUnlocked := make(map[string]bool)
	for _, diagnostic := range m.modelDiagnostics {
		model := strings.TrimPrefix(diagnostic.Name, "model:")
		models[model] = true
		modelUnlocked[model] = diagnostic.Unlocked
	}
	for _, diagnostic := range m.typeDiagnostics {
		typeUnlocked[strings.TrimPrefix(diagnostic.Name, "type:")] = diagnostic.Unlocked
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
			if fixed := m.fixedModels[model]; fixed > 0 {
				estimate.Value *= fixed
				estimate.Low *= fixed
				estimate.High *= fixed
				estimate.PriorLocked = true
				estimate.Identified = false
			}
			if model != weightReferenceModel && !modelUnlocked[model] || typ != "input" && !typeUnlocked[typ] {
				estimate.PriorLocked = true
				estimate.Identified = false
			}
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

func deriveCycleScales(m weightModel, x []float64, covariance [][]float64) []learnedCycleScale {
	result := make([]learnedCycleScale, 0, len(m.cycles))
	for _, cycle := range m.cycles {
		estimate := deriveFactorEstimate(m, x, covariance, []string{"scale:" + cycle.Key}, 1)
		inverse := weightEstimate{Value: 100 / estimate.Value, Low: 100 / estimate.High, High: 100 / estimate.Low,
			Identified: estimate.Identified, DataShare: estimate.DataShare, Correlated: estimate.Correlated}
		result = append(result, learnedCycleScale{Account: cycle.Account, Window: cycle.Window, CycleID: cycle.CycleID,
			RegimeResetAt: cycle.RegimeResetAt, StartedAt: cycle.StartedAt, Scale: estimate, CreditsPerPercent: inverse})
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
