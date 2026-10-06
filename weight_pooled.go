package main

import (
	"math"
	"sort"
	"strings"
)

type pooledComponentSupport struct {
	Tokens    int64   `json:"tokens"`
	MeanShare float64 `json:"mean_share"`
	MinShare  float64 `json:"min_share"`
	MaxShare  float64 `json:"max_share"`
}

type pooledValidationEvidence struct {
	TrainingSegments   int     `json:"training_segments"`
	ValidationSegments int     `json:"validation_segments"`
	PriorMAE           float64 `json:"prior_mae"`
	CandidateMAE       float64 `json:"candidate_mae"`
	Accepted           bool    `json:"accepted"`
	Reason             string  `json:"reason"`
}

type pooledModelEstimate struct {
	Model       string                            `json:"model"`
	Estimate    weightEstimate                    `json:"estimate"`
	Applied     bool                              `json:"applied"`
	Diagnostic  identifiabilityDiagnostic         `json:"diagnostic"`
	Composition map[string]pooledComponentSupport `json:"composition"`
	Evidence    pooledValidationEvidence          `json:"evidence"`
}

func pooledParameter(model string) string { return "pooled:" + normalizeModel(model) }

type contrastColumn struct{ name, model, typ string }

func contrastMatches(column contrastColumn, f segmentFeature) bool {
	switch column.typ {
	case "fast":
		return f.Fast
	case "long":
		return f.Long
	case "fast_long":
		return f.Fast && f.Long
	case "total":
		return normalizeModel(f.Model) == column.model
	default:
		return normalizeModel(f.Model) == column.model && f.Type == column.typ
	}
}

// Conditional contrasts use one coefficient per model total, never its three
// proportional columns as simultaneous nuisance parameters for that total.
func assessPooledAndModifierContrasts(segments []quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions, components []identifiabilityDiagnostic) ([]identifiabilityDiagnostic, []identifiabilityDiagnostic) {
	var specs []contrastColumn
	independent := map[string]bool{}
	for _, d := range components {
		if d.Unlocked {
			independent[d.Model] = true
			if d.Model == weightReferenceModel {
				specs = append(specs, contrastColumn{d.Name, d.Model, d.TokenType})
			}
		}
	}
	for _, model := range assessedModelNames(segments, prices) {
		if independent[model] {
			for _, d := range components {
				if d.Model == model && d.Unlocked {
					specs = append(specs, contrastColumn{d.Name, model, d.TokenType})
				}
			}
		} else {
			specs = append(specs, contrastColumn{pooledParameter(model), model, "total"})
		}
	}
	specs = append(specs, contrastColumn{"fast", "", "fast"}, contrastColumn{"long", "", "long"}, contrastColumn{"fast_long", "", "fast_long"})
	columns := make([][]float64, len(specs))
	diagnostics := make([]identifiabilityDiagnostic, len(specs))
	denominator := referenceSolRate(pricingModeCredits, prices)
	for index, spec := range specs {
		columns[index] = make([]float64, len(segments))
		diagnostics[index] = assessWeightParameter(segments, prices, now, opts, spec.name, func(f segmentFeature) bool { return contrastMatches(spec, f) }, opts.ModelShareMinSD, opts.ModelMinFisher)
		diagnostics[index].Model = spec.model
	}
	cycles := map[string][]int{}
	for index, segment := range segments {
		cycles[modelCycle(segment)] = append(cycles[modelCycle(segment)], index)
	}
	for _, rows := range cycles {
		var ratios []float64
		for _, index := range rows {
			s := segments[index]
			total := referenceEquivalent(s, pricingModeCredits, prices)
			if total > 0 {
				ratios = append(ratios, s.DP/total)
			}
		}
		scale := quantile(ratios, .5)
		nuisance := make([]float64, len(rows))
		var norm float64
		for j, index := range rows {
			s := segments[index]
			root := math.Sqrt(segmentFitWeight(s, now, opts.HalfLifeDays))
			nuisance[j] = root * scale * referenceEquivalent(s, pricingModeCredits, prices)
			norm += nuisance[j] * nuisance[j]
			for _, f := range s.Features {
				value := root * scale * float64(f.Tokens) / 1_000_000 * referenceFeatureRate(f, pricingModeCredits, prices) / denominator
				for k, spec := range specs {
					if contrastMatches(spec, f) {
						columns[k][index] += value
					}
				}
			}
		}
		if norm > 0 {
			for _, column := range columns {
				var dot float64
				for j, index := range rows {
					dot += column[index] * nuisance[j]
				}
				for j, index := range rows {
					column[index] -= dot / norm * nuisance[j]
				}
			}
		}
	}
	fastIndex, longIndex := len(specs)-3, len(specs)-2
	var cross float64
	for i := range segments {
		cross += columns[fastIndex][i] * columns[longIndex][i]
	}
	fastNorm, longNorm := componentSquaredNorm(columns[fastIndex]), componentSquaredNorm(columns[longIndex])
	confounded := fastNorm > 1e-12 && longNorm > 1e-12 && cross/math.Sqrt(fastNorm*longNorm) > 1-1e-8
	// Proportional aggregate columns do not prove causal co-exposure. Fixed
	// mixtures of separate modes can also produce identical derivatives.
	if confounded {
		for _, rows := range cycles {
			var information float64
			for _, index := range rows {
				information += columns[fastIndex][index]*columns[fastIndex][index] + columns[longIndex][index]*columns[longIndex][index]
			}
			if information <= 1e-12 {
				continue
			}
			for _, index := range rows {
				for _, feature := range segments[index].Features {
					if feature.Tokens > 0 && feature.Fast != feature.Long {
						confounded = false
					}
				}
			}
		}
	}
	if confounded {
		last := len(specs) - 1
		specs = append(specs[:fastIndex], specs[last])
		columns = append(columns[:fastIndex], columns[last])
		diagnostics = append(diagnostics[:fastIndex], diagnostics[last])
	} else {
		specs, columns, diagnostics = specs[:len(specs)-1], columns[:len(columns)-1], diagnostics[:len(diagnostics)-1]
	}
	for target := range specs {
		var basis [][]float64
		for other, column := range columns {
			if other == target || opts.FixedModelFactors[specs[other].model] > 0 {
				continue
			}
			v := append([]float64(nil), column...)
			original := componentSquaredNorm(v)
			projectComponentVector(v, basis)
			norm := componentSquaredNorm(v)
			if norm <= math.Max(1e-14, original*1e-10) {
				continue
			}
			norm = math.Sqrt(norm)
			for j := range v {
				v[j] /= norm
			}
			basis = append(basis, v)
		}
		residual := append([]float64(nil), columns[target]...)
		projectComponentVector(residual, basis)
		d := &diagnostics[target]
		d.ConditionalFisher = componentSquaredNorm(residual) / (weightObservationSD * weightObservationSD)
		d.Unlocked = d.WithinCycleShareSD >= d.MinShareSD && d.ConditionalFisher >= d.MinFisher
	}
	var pooled, modifiers []identifiabilityDiagnostic
	for _, d := range diagnostics {
		if strings.HasPrefix(d.Name, "pooled:") {
			pooled = append(pooled, d)
		} else if d.Name == "fast" || d.Name == "long" || d.Name == "fast_long" {
			modifiers = append(modifiers, d)
		}
	}
	return pooled, modifiers
}

func observedPooledComposition(model string, segments []quotaSegment, prices map[string]price) map[string]pooledComponentSupport {
	result := map[string]pooledComponentSupport{}
	var count int
	for _, s := range segments {
		values := map[string]float64{}
		var total float64
		for _, f := range s.Features {
			if normalizeModel(f.Model) == model {
				value := float64(f.Tokens) * referenceFeatureRate(segmentFeature{Model: model, Type: f.Type}, pricingModeCredits, prices)
				values[f.Type] += value
				total += value
				r := result[f.Type]
				r.Tokens += f.Tokens
				result[f.Type] = r
			}
		}
		if total <= 0 {
			continue
		}
		count++
		for _, typ := range weightTokenTypes {
			share := values[typ] / total
			r := result[typ]
			if count == 1 {
				r.MinShare, r.MaxShare = share, share
			} else {
				r.MinShare = math.Min(r.MinShare, share)
				r.MaxShare = math.Max(r.MaxShare, share)
			}
			r.MeanShare += share
			result[typ] = r
		}
	}
	if count > 0 {
		for typ, r := range result {
			r.MeanShare /= float64(count)
			result[typ] = r
		}
	}
	return result
}

func sortedPooledModels(models map[string]pooledModelEstimate) []pooledModelEstimate {
	var names []string
	for model := range models {
		names = append(names, model)
	}
	sort.Strings(names)
	result := make([]pooledModelEstimate, 0, len(names))
	for _, model := range names {
		result = append(result, models[model])
	}
	return result
}

// Pick the cutoff from this model's exposure history. Background observations
// remain available to fit capacity, but cannot put every cold-model observation
// on the validation side. Paired policies update capacity AFTER prediction.
func pooledValidation(model string, segments []quotaSegment, prices map[string]price, opts weightLearnerOptions) pooledValidationEvidence {
	model = normalizeModel(model)
	sorted := append([]quotaSegment(nil), segments...)
	sortSegmentsChronologically(sorted)
	evidence := pooledValidationEvidence{Reason: "insufficient_validation"}
	var exposureTimes []int64
	for _, segment := range sorted {
		for _, feature := range segment.Features {
			if feature.Tokens > 0 && normalizeModel(feature.Model) == model {
				exposureTimes = append(exposureTimes, segment.EndAt)
				break
			}
		}
	}
	// A two-thirds split needs ten target observations to leave at least four
	// validation observations. This does not lower the existing admission gate.
	if len(exposureTimes) < 10 {
		return evidence
	}
	cutoff := exposureTimes[len(exposureTimes)*2/3]
	// Keep simultaneous observations together, including related quota windows.
	split := sort.Search(len(sorted), func(i int) bool { return sorted[i].EndAt >= cutoff })
	training, validation := sorted[:split], sorted[split:]
	evidence.TrainingSegments = len(training)
	if len(training) < 5 {
		return evidence
	}
	internal := opts
	internal.SkipPooledValidation = true
	internal.PooledDiagnosticOnly = false
	internal.ApprovedPooledModels = map[string]bool{model: true}
	candidate, err := fitQuotaWeights(training, prices, training[len(training)-1].EndAt, internal)
	if err != nil || !candidate.Available {
		return evidence
	}
	applied := false
	for _, pool := range candidate.PooledModels {
		if pool.Model == model && pool.Applied {
			applied = true
		}
	}
	if !applied {
		evidence.Reason = "training_not_identified"
		return evidence
	}
	internal.DisablePooled = true
	internal.ApprovedPooledModels = nil
	baseline, err := fitQuotaWeights(training, prices, training[len(training)-1].EndAt, internal)
	if err != nil || !baseline.Available {
		return evidence
	}
	candidateTracker := newOnlineScaleTracker(candidate, opts.RandomWalkSigma)
	priorTracker := newOnlineScaleTracker(baseline, opts.RandomWalkSigma)
	var candidateError, priorError float64
	for _, s := range validation {
		candidateValue, priorValue := learnedSegmentEquivalent(s, candidate), learnedSegmentEquivalent(s, baseline)
		if candidateValue <= 0 || priorValue <= 0 {
			continue
		}
		candidateState, priorState := candidateTracker.forSegment(s), priorTracker.forSegment(s)
		relevant := false
		for _, f := range s.Features {
			if normalizeModel(f.Model) == model && f.Tokens > 0 {
				relevant = true
			}
		}
		if relevant {
			candidateError += math.Abs(math.Exp(candidateState.LogValue)*candidateValue - s.DP)
			priorError += math.Abs(math.Exp(priorState.LogValue)*priorValue - s.DP)
			evidence.ValidationSegments++
		}
		weight := segmentFitWeight(s, s.EndAt, opts.HalfLifeDays)
		candidateState.update(candidateValue, s.DP, weight, 0)
		priorState.update(priorValue, s.DP, weight, 0)
	}
	if evidence.ValidationSegments > 0 {
		evidence.CandidateMAE = candidateError / float64(evidence.ValidationSegments)
		evidence.PriorMAE = priorError / float64(evidence.ValidationSegments)
	}
	if evidence.ValidationSegments < 4 {
		return evidence
	}
	evidence.Accepted = evidence.CandidateMAE < evidence.PriorMAE
	if evidence.Accepted {
		evidence.Reason = "validation_improved"
	} else {
		evidence.Reason = "validation_not_improved"
	}
	return evidence
}
