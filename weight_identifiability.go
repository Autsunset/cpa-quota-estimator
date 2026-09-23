package main

import (
	"math"
	"sort"
)

type fisherCycleSums struct {
	Weight, Share, ShareSquared        float64
	TotalSquared, Cross, TargetSquared float64
	Ratios                             []float64
}

func weightOptionsWithDefaults(opts weightLearnerOptions) weightLearnerOptions {
	defaults := defaultWeightLearnerOptions()
	if opts.HalfLifeDays <= 0 {
		opts.HalfLifeDays = defaults.HalfLifeDays
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = defaults.MaxIterations
	}
	if opts.RandomWalkSigma <= 0 {
		opts.RandomWalkSigma = defaults.RandomWalkSigma
	}
	if opts.CacheShareMinSD <= 0 {
		opts.CacheShareMinSD = defaults.CacheShareMinSD
	}
	if opts.OutputShareMinSD <= 0 {
		opts.OutputShareMinSD = defaults.OutputShareMinSD
	}
	if opts.TypeMinFisher <= 0 {
		opts.TypeMinFisher = defaults.TypeMinFisher
	}
	if opts.ModelShareMinSD <= 0 {
		opts.ModelShareMinSD = defaults.ModelShareMinSD
	}
	if opts.ModelMinFisher <= 0 {
		opts.ModelMinFisher = defaults.ModelMinFisher
	}
	if opts.CacheCycleRatioMinRange <= 0 {
		opts.CacheCycleRatioMinRange = defaults.CacheCycleRatioMinRange
	}
	if opts.OutputCycleRatioMinRange <= 0 {
		opts.OutputCycleRatioMinRange = defaults.OutputCycleRatioMinRange
	}
	return opts
}

func assessedModelNames(segments []quotaSegment, prices map[string]price) []string {
	models := map[string]bool{}
	for _, segment := range segments {
		for _, feature := range segment.Features {
			model := normalizeModel(feature.Model)
			if model != weightReferenceModel && referenceFeatureRate(feature, pricingModeCredits, prices) > 0 {
				models[model] = true
			}
		}
	}
	for _, model := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if referenceFeatureRate(segmentFeature{Model: model, Type: "input"}, pricingModeCredits, prices) > 0 {
			models[model] = true
		}
	}
	var names []string
	for model := range models {
		names = append(names, model)
	}
	sort.Strings(names)
	return names
}

func assessWeightParameter(segments []quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions,
	name string, match func(segmentFeature) bool, minShareSD, minFisher float64) identifiabilityDiagnostic {
	result := identifiabilityDiagnostic{Name: name, MinShareSD: minShareSD, MinFisher: minFisher}
	cycles := make(map[string]*fisherCycleSums)
	for _, segment := range segments {
		denominator := referenceSolRate(pricingModeCredits, prices)
		if denominator <= 0 {
			continue
		}
		var total, target float64
		for _, feature := range segment.Features {
			value := float64(feature.Tokens) / 1_000_000 * referenceFeatureRate(feature, pricingModeCredits, prices) / denominator
			total += value
			if match(feature) {
				target += value
			}
		}
		if total <= 0 {
			continue
		}
		cycle := modelCycle(segment)
		group := cycles[cycle]
		if group == nil {
			group = &fisherCycleSums{}
			cycles[cycle] = group
		}
		group.Ratios = append(group.Ratios, segment.DP/total)
	}
	var weightedVariance, totalWeight, fisher float64
	for cycle, group := range cycles {
		sort.Float64s(group.Ratios)
		scale := group.Ratios[len(group.Ratios)/2]
		for _, segment := range segments {
			if modelCycle(segment) != cycle {
				continue
			}
			var total, target float64
			denominator := referenceSolRate(pricingModeCredits, prices)
			for _, feature := range segment.Features {
				value := float64(feature.Tokens) / 1_000_000 * referenceFeatureRate(feature, pricingModeCredits, prices) / denominator
				total += value
				if match(feature) {
					target += value
				}
			}
			if total <= 0 {
				continue
			}
			weight := segmentFitWeight(segment, now, opts.HalfLifeDays)
			share := target / total
			group.Weight += weight
			group.Share += weight * share
			group.ShareSquared += weight * share * share
			totalDerivative := scale * total
			targetDerivative := scale * target
			group.TotalSquared += weight * totalDerivative * totalDerivative
			group.Cross += weight * totalDerivative * targetDerivative
			group.TargetSquared += weight * targetDerivative * targetDerivative
		}
		if group.Weight > 0 {
			weightedVariance += group.ShareSquared - group.Share*group.Share/group.Weight
			totalWeight += group.Weight
		}
		if group.TotalSquared > 0 {
			fisher += (group.TargetSquared - group.Cross*group.Cross/group.TotalSquared) / (weightObservationSD * weightObservationSD)
		}
	}
	if totalWeight > 0 {
		result.WithinCycleShareSD = math.Sqrt(math.Max(0, weightedVariance/totalWeight))
	}
	result.ConditionalFisher = math.Max(0, fisher)
	result.Unlocked = result.WithinCycleShareSD >= minShareSD && result.ConditionalFisher >= minFisher
	return result
}

func assessWeightIdentifiability(segments []quotaSegment, prices map[string]price, now int64, opts weightLearnerOptions) ([]identifiabilityDiagnostic, []identifiabilityDiagnostic) {
	opts = weightOptionsWithDefaults(opts)
	typeSegments := make([]quotaSegment, 0, len(segments))
	for _, segment := range segments {
		if segment.Window == mainQuotaScope {
			typeSegments = append(typeSegments, segment)
		}
	}
	if len(typeSegments) == 0 {
		typeSegments = segments
	}
	types := []identifiabilityDiagnostic{
		assessWeightParameter(typeSegments, prices, now, opts, "type:cache", func(f segmentFeature) bool { return f.Type == "cache" }, opts.CacheShareMinSD, opts.TypeMinFisher),
		assessWeightParameter(typeSegments, prices, now, opts, "type:output", func(f segmentFeature) bool { return f.Type == "output" }, opts.OutputShareMinSD, opts.TypeMinFisher),
	}
	for i := range types {
		typ, threshold := "cache", opts.CacheCycleRatioMinRange
		if i == 1 {
			typ, threshold = "output", opts.OutputCycleRatioMinRange
		}
		types[i].CycleRatioRange, types[i].CyclesCompared = cycleTokenRatioRange(typeSegments, typ)
		types[i].MinCycleRatioRange = threshold
		types[i].Unlocked = types[i].Unlocked && types[i].CycleRatioRange >= threshold
	}
	models := make([]identifiabilityDiagnostic, 0)
	for _, model := range assessedModelNames(segments, prices) {
		modelCopy := model
		models = append(models, assessWeightParameter(segments, prices, now, opts, "model:"+model,
			func(f segmentFeature) bool { return normalizeModel(f.Model) == modelCopy }, opts.ModelShareMinSD, opts.ModelMinFisher))
	}
	return types, models
}

func cycleTokenRatioRange(segments []quotaSegment, typ string) (float64, int) {
	type totals struct {
		input, cache, output int64
		segments             int
	}
	byCycle := make(map[string]*totals)
	for _, segment := range segments {
		key := modelCycle(segment)
		row := byCycle[key]
		if row == nil {
			row = &totals{}
			byCycle[key] = row
		}
		row.segments++
		for _, feature := range segment.Features {
			switch feature.Type {
			case "input":
				row.input += feature.Tokens
			case "cache":
				row.cache += feature.Tokens
			case "output":
				row.output += feature.Tokens
			}
		}
	}
	var ratios []float64
	for _, row := range byCycle {
		input := row.input + row.cache
		if row.segments < 5 || input < 1_000_000 {
			continue
		}
		if typ == "cache" {
			ratios = append(ratios, float64(row.cache)/float64(input))
		}
		if typ == "output" {
			ratios = append(ratios, float64(row.output)/float64(input))
		}
	}
	if len(ratios) < 2 {
		return 0, len(ratios)
	}
	sort.Float64s(ratios)
	return ratios[len(ratios)-1] - ratios[0], len(ratios)
}
