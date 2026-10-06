package main

import (
	"math"
	"sort"
)

var weightTokenTypes = []string{"input", "cache", "output"}

func componentParameter(model, typ string) string {
	return "component:" + normalizeModel(model) + ":" + typ
}

// Each column is a model/token-type contribution. Project out independent
// cycle capacities, then all other token components and speed/context effects.
// This prevents a cache-heavy model from identifying its input/output rates.
func assessComponentIdentifiability(segments []quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions) []identifiabilityDiagnostic {
	models := append([]string{weightReferenceModel}, assessedModelNames(segments, prices)...)
	var diagnostics []identifiabilityDiagnostic
	for _, model := range models {
		for _, typ := range weightTokenTypes {
			// Only the reference input fixes the gauge; its cache/output
			// ratios must remain learnable from composition contrasts.
			if model == weightReferenceModel && typ == "input" {
				continue
			}
			model, typ := model, typ
			d := assessWeightParameter(segments, prices, now, opts, componentParameter(model, typ),
				func(f segmentFeature) bool { return normalizeModel(f.Model) == model && f.Type == typ }, opts.ModelShareMinSD, opts.ModelMinFisher)
			d.Model, d.TokenType = model, typ
			diagnostics = append(diagnostics, d)
		}
	}
	n := len(diagnostics)
	columns := make([][]float64, n+2)
	for i := range columns {
		columns[i] = make([]float64, len(segments))
	}
	indices := make(map[string]int)
	for i, d := range diagnostics {
		indices[d.Name] = i
	}
	cycleIndices := make(map[string][]int)
	ratios := make(map[string][]float64)
	denominator := referenceSolRate(pricingModeCredits, prices)
	for i, s := range segments {
		total := referenceEquivalent(s, pricingModeCredits, prices)
		if total <= 0 || denominator <= 0 {
			continue
		}
		key := modelCycle(s)
		cycleIndices[key] = append(cycleIndices[key], i)
		ratios[key] = append(ratios[key], s.DP/total)
	}
	for cycle, rows := range cycleIndices {
		sort.Float64s(ratios[cycle])
		scale := quantile(ratios[cycle], .5)
		nuisance := make([]float64, len(rows))
		var norm float64
		for j, i := range rows {
			s := segments[i]
			rootWeight := math.Sqrt(segmentFitWeight(s, now, opts.HalfLifeDays))
			nuisance[j] = rootWeight * scale * referenceEquivalent(s, pricingModeCredits, prices)
			norm += nuisance[j] * nuisance[j]
			for _, f := range s.Features {
				value := rootWeight * scale * float64(f.Tokens) / 1_000_000 * referenceFeatureRate(f, pricingModeCredits, prices) / denominator
				if index, ok := indices[componentParameter(f.Model, f.Type)]; ok {
					columns[index][i] += value
				}
				if f.Fast {
					columns[n][i] += value
				}
				if f.Long {
					columns[n+1][i] += value
				}
			}
		}
		if norm > 0 {
			for _, column := range columns {
				var dot float64
				for j, i := range rows {
					dot += column[i] * nuisance[j]
				}
				for j, i := range rows {
					column[i] -= dot / norm * nuisance[j]
				}
			}
		}
	}
	for target := range diagnostics {
		var basis [][]float64
		for other, column := range columns {
			if other == target {
				continue
			}
			if other < n && opts.FixedModelFactors[diagnostics[other].Model] > 0 {
				continue
			}
			// Reference diagnostics run first. A reference component that
			// remains fixed at its prior is not a new fitted nuisance for
			// other models; adding it would discard their existing evidence.
			if other < n && diagnostics[target].Model != weightReferenceModel &&
				diagnostics[other].Model == weightReferenceModel && !diagnostics[other].Unlocked {
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
			for i := range v {
				v[i] /= norm
			}
			basis = append(basis, v)
		}
		residual := append([]float64(nil), columns[target]...)
		projectComponentVector(residual, basis)
		diagnostics[target].ConditionalFisher = componentSquaredNorm(residual) / (weightObservationSD * weightObservationSD)
		d := &diagnostics[target]
		d.Unlocked = d.WithinCycleShareSD >= d.MinShareSD && d.ConditionalFisher >= d.MinFisher
	}
	return diagnostics
}

func componentSquaredNorm(v []float64) float64 {
	var result float64
	for _, value := range v {
		result += value * value
	}
	return result
}

func projectComponentVector(v []float64, basis [][]float64) {
	// Reorthogonalize to keep nearly proportional columns from producing false
	// information through cancellation or the order of the nuisance columns.
	for pass := 0; pass < 2; pass++ {
		for _, q := range basis {
			var dot float64
			for i := range v {
				dot += v[i] * q[i]
			}
			for i := range v {
				v[i] -= dot * q[i]
			}
		}
	}
}
