package main

import (
	"math"
	"testing"
)

func TestReviewerAnchorIsOneUnitConversionForWholeWorkload(t *testing.T) {
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeAPI
	cfg.LongContextThreshold = 2_000_000
	cfg.LearnedFit = testAnchorFit()
	// Distinct learned ratios expose the old component-by-component anchoring error.
	cfg.LearnedFit.Models[1].Cache = weightEstimate{Value: .2, Low: .18, High: .22, Identified: true}
	cfg.LearnedFit.Models[1].Output = weightEstimate{Value: 20, Low: 18, High: 22, Identified: true}
	cfg.PriceCatalog = map[string]price{
		weightReferenceModel: {Model: weightReferenceModel, Input: 4, CacheRead: .4, Output: 20, CacheWrite: 5},
		"gpt-6-astra":        {Model: "gpt-6-astra", Input: 10, CacheRead: 1, Output: 50, CacheWrite: 12.5},
		"gpt-6-sol":          {Model: "gpt-6-sol", Input: 2, CacheRead: .2, Output: 10, CacheWrite: 2.5},
	}
	before := map[string]price{}
	cfg.AnchorModel = weightReferenceModel
	for model, p := range cfg.PriceCatalog {
		before[model], _ = cfg.effectiveModelPrice(p)
	}
	cfg.AnchorModel = "gpt-6-astra"
	for model, p := range cfg.PriceCatalog {
		after, _ := cfg.effectiveModelPrice(p)
		old := before[model]
		for component, pair := range map[string][2]float64{"input": {old.Input, after.Input}, "cache": {old.CacheRead, after.CacheRead}, "output": {old.Output, after.Output}, "cache_write": {old.CacheWrite, after.CacheWrite}} {
			if math.Abs(pair[1]*1.2-pair[0]) > 1e-9 {
				t.Errorf("%s/%s changed relative shape: before=%g after=%g; all must divide by 1.2", model, component, pair[0], pair[1])
			}
		}
	}
	anchor := cfg.priceRow(cfg.PriceCatalog["gpt-6-astra"])
	if !anchor.ComponentAdjustments["input"].Baseline || anchor.ComponentAdjustments["cache_read"].Baseline || anchor.ComponentAdjustments["output"].Baseline {
		t.Fatal("only anchor input is the unit convention")
	}
}

func TestReviewerRejectedModifiersCannotAffectPublicValuation(t *testing.T) {
	p, _ := officialGPT6Price("gpt-6.1-sol")
	for _, mode := range []string{pricingModeAPI, pricingModeCredits} {
		original := defaultConfig()
		original.PricingMode = mode
		original.PriceCatalog = map[string]price{p.Model: p}
		d := usageDetail{InputTokens: 1_000_000, CacheReadTokens: 250_000, OutputTokens: 120_000}
		baseline := calculateCost(p, d, "fast", original)
		for _, modifier := range []weightEstimate{
			{Value: 7, Low: 6, High: 8},
			{Value: 7, Low: 6, High: 8, Identified: true, Correlated: true},
			{Value: 7, Low: 6, High: 8, Identified: true, PriorLocked: true},
		} {
			cfg := original
			cfg.LearnedFit = &weightFit{Available: true, Fast: modifier, LongContext: modifier}
			if got := calculateCost(p, d, "fast", cfg); math.Abs(got-baseline) > 1e-9 {
				t.Errorf("%s rejected modifier used: got=%g want=%g", mode, got, baseline)
			}
		}
	}
}

func TestReviewerStoredFitAndRuntimePredictionAgree(t *testing.T) {
	for _, count := range []int{10, 30, 100} {
		segments := componentSyntheticSegments(count, false)
		for i := range segments {
			segments[i].CycleID = int64(1 + i/5)
			segments[i].DP *= 1 + .15*float64(i/5)
		}
		fit, err := fitQuotaWeights(segments, nil, 1800000000, defaultWeightLearnerOptions())
		if err != nil || !fit.Available {
			t.Fatalf("count=%d: fit available=%v err=%v", count, fit.Available, err)
		}
		for i, s := range segments {
			scale, ok := learnedScale(&fit, s.Account, s.Window, s.CycleID, s.RegimeResetAt)
			if !ok {
				t.Fatal("missing cycle scale")
			}
			stored := fit.predict(s, nil)
			runtime := learnedSegmentEquivalent(s, fit) * scale
			if math.Abs(stored-runtime) > 1e-5*math.Max(1, math.Abs(stored)) {
				t.Fatalf("count=%d segment=%d: fitted prediction=%g runtime=%g", count, i, stored, runtime)
			}
		}
	}
}

func TestReviewerCapacityPosteriorSurvivesNuisanceCorrelation(t *testing.T) {
	m := weightModel{
		names:     []string{"scale:a|main|1|1", "scale:a|main|2|2"},
		index:     map[string]int{"scale:a|main|1|1": 0, "scale:a|main|2|2": 1},
		priorMean: []float64{0, 0}, priorSigma: []float64{2.5, 2.5},
		cycles: []learningCycle{{Key: "a|main|1|1", Account: "a", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 1}, {Key: "a|main|2|2", Account: "a", Window: mainQuotaScope, CycleID: 2, RegimeResetAt: 2}},
	}
	// Adjacent cycle capacities legitimately share a smoothing prior.
	// Their correlation must not discard observed capacity information.
	scales := deriveCycleScales(m, []float64{math.Log(2), math.Log(3)}, [][]float64{{.01, .0099}, {.0099, .01}})
	if len(scales) != 2 || math.Abs(scales[0].Scale.Value-2) > 1e-9 || math.Abs(scales[1].Scale.Value-3) > 1e-9 {
		t.Fatalf("correlated capacities were reset: %+v", scales)
	}
}

func TestReviewerOfficialFastCreditBreakdownMatchesPublishedValuation(t *testing.T) {
	d := usageDetail{InputTokens: 100000, CacheReadTokens: 20000, OutputTokens: 10000}
	for _, model := range []string{"gpt-6.1-sol", "gpt-6-sol", "gpt-6-astra", "gpt-5.6-sol"} {
		standard, ok := officialCreditsForUsage(model, "standard", d.InputTokens, d.CacheReadTokens, 0, d.OutputTokens)
		fast, fastOK := officialCreditsForUsage(model, "fast", d.InputTokens, d.CacheReadTokens, 0, d.OutputTokens)
		if !ok || !fastOK || math.Abs(fast-2*standard) > 1e-9 {
			t.Errorf("%s paid credits Fast=%g known=%v; want 2x Standard=%g", model, fast, fastOK, standard)
		}
		p := price{Model: model}
		cfg := defaultConfig()
		cfg.PricingMode = pricingModeCredits
		if got := calculateCost(p, d, "fast", cfg); math.Abs(got-fast) > 1e-9 {
			t.Errorf("%s price=%g breakdown=%g", model, got, fast)
		}
	}
}

func TestReviewerAPIReferenceUsesPublishedFastTier(t *testing.T) {
	p, _ := officialGPT6Price("gpt-6.1-sol")
	prices := map[string]price{p.Model: p}
	for _, typ := range []string{"input", "cache", "output"} {
		normal := referenceFeatureRate(segmentFeature{Model: p.Model, Type: typ}, pricingModeAPI, prices)
		fast := referenceFeatureRate(segmentFeature{Model: p.Model, Type: typ, Fast: true}, pricingModeAPI, prices)
		if math.Abs(fast-2*normal) > 1e-9 {
			t.Errorf("%s API reference Fast=%g Standard=%g; expected 2x", typ, fast, normal)
		}
	}
}

func TestReviewerPooledAttributionIsAdditiveAndDoesNotInventOutputEvidence(t *testing.T) {
	model := "gpt-6.1-sol"
	fit := &weightFit{EligibilityVersion: weightEligibilityVersion, Available: true,
		Models: []learnedModelWeights{{Model: model, Input: weightEstimate{Source: "pooled", Value: .5 * 1.4, Low: .5 * 1.2 * math.Exp(-.98), High: .5 * 1.6 * math.Exp(.98), PriorLocked: true}, Cache: weightEstimate{Source: "pooled", Value: .025 * 1.4, Low: .025 * 1.2 * math.Exp(-.98), High: .025 * 1.6 * math.Exp(.98), PriorLocked: true}, Output: priorWeightEstimate(2.5, .5)}},
		PooledModels: []pooledModelEstimate{{Model: model, Applied: true, Estimate: weightEstimate{Source: "pooled", Value: 1.4, Low: 1.2, High: 1.6, Identified: true, DataShare: .8},
			Composition: map[string]pooledComponentSupport{"input": {Tokens: 500000, MeanShare: .8, MinShare: .8, MaxShare: .8}, "cache": {Tokens: 1000000, MeanShare: .2, MinShare: .2, MaxShare: .2}},
			Evidence:    pooledValidationEvidence{Accepted: true, TrainingSegments: 20, ValidationSegments: 10, PriorMAE: .2, CandidateMAE: .1}}},
	}
	// Pure requests differ from the segment's average profile. Their sum must
	// still match that segment; no per-request min/max composition gate is valid.
	pieces := []usageDetail{{InputTokens: 500000}, {InputTokens: 1000000, CacheReadTokens: 1000000}, {OutputTokens: 20000}}
	combined := usageDetail{InputTokens: 1500000, CacheReadTokens: 1000000, OutputTokens: 20000}
	var sum float64
	for _, d := range pieces {
		v, ok := learnedEquivalentForUsage(fit, model, d, "", 2000000)
		if !ok {
			t.Fatal("missing pooled attribution")
		}
		sum += v
	}
	aggregate, ok := learnedEquivalentForUsage(fit, model, combined, "", 2000000)
	if !ok {
		t.Fatal("missing aggregate")
	}
	segment := quotaSegment{Features: []segmentFeature{{Model: model, Type: "input", Tokens: 500000}, {Model: model, Type: "cache", Tokens: 1000000}, {Model: model, Type: "output", Tokens: 20000}}}
	want := (500000*.5*1.4 + 1000000*.025*1.4 + 20000*2.5) / 1000000
	for label, value := range map[string]float64{"request sum": sum, "aggregate": aggregate, "segment": learnedSegmentEquivalent(segment, *fit)} {
		if math.Abs(value-want) > 1e-9 {
			t.Errorf("%s=%g want=%g", label, value, want)
		}
	}
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeCredits
	cfg.LongContextThreshold = 2000000
	cfg.LearnedFit = fit
	p, _ := officialGPT6Price(model)
	cfg.PriceCatalog = map[string]price{model: p}
	if got := calculateCost(p, combined, "", cfg); math.Abs(got-want*100) > 1e-9 {
		t.Errorf("public pooled valuation=%g, quota-equivalent reference=%g", got, want*100)
	}
	row := cfg.priceRow(p)
	if row.ComponentAdjustments["input"].Calibrated || row.ComponentAdjustments["cache_read"].Calibrated || row.ComponentAdjustments["output"].Calibrated {
		t.Fatal("pooled evidence falsely became independent component calibration")
	}
	if math.Abs(row.Calculated.Output-row.Official.Output) > 1e-9 {
		t.Fatal("unobserved output inherited pooled multiplier")
	}
}

func TestReviewerChronologicalEvidenceControlsPooledApplication(t *testing.T) {
	for _, improves := range []bool{true, false} {
		segments := pooledContrastSegments(36)
		if !improves {
			// The candidate fitted to earlier data is wrong in the later period.
			// The final full-data fit must not hide that failed validation.
			for i := 24; i < len(segments); i++ {
				if i%2 == 1 {
					segments[i].DP = 1.5
				}
			}
		}
		fit, err := fitQuotaWeights(segments, nil, 1800000100, defaultWeightLearnerOptions())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, pool := range fit.PooledModels {
			if pool.Model != "gpt-6.1-sol" {
				continue
			}
			found = true
			if pool.Applied != improves || pool.Evidence.Accepted != improves || pool.Evidence.ValidationSegments < 4 {
				t.Errorf("improves=%v but pool=%+v", improves, pool)
			}
		}
		if !found {
			t.Fatal("missing candidate evidence")
		}
	}
}

func TestReviewerCombinedModifierNeverLeaksIntoStandaloneModes(t *testing.T) {
	var segments []quotaSegment
	for i := 0; i < 20; i++ {
		combined := i%2 == 1
		tokens, dp := int64(1000000), 1.0
		if combined {
			tokens, dp = 400000, 1.2
		}
		segments = append(segments, quotaSegment{Account: "a", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 1900000000, EndAt: 1800000000 + int64(i), DP: dp, BoundaryWeight: 1, Features: []segmentFeature{{Model: weightReferenceModel, Type: "input", Tokens: tokens, Fast: combined, Long: combined}}})
	}
	fit, err := fitQuotaWeights(segments, nil, 1800000100, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	if fit.FastLong == nil || !isFinitePositive(fit.FastLong.Value) || math.Abs(fit.FastLong.Value/3-1) > .1 {
		t.Fatalf("combined 3x ratio not recovered: %+v", fit.FastLong)
	}
	if fit.Fast.Identified || fit.LongContext.Identified {
		t.Fatal("combined evidence falsely calibrated standalone modifiers")
	}
	for _, tc := range []struct {
		name, tier string
		tokens     int64
		want       float64
	}{
		{"standard", "", 100000, .1}, {"fast only", "fast", 100000, .25}, {"long only", "", 400000, .4}, {"both", "fast", 400000, .4 * fit.FastLong.Value},
	} {
		got, ok := learnedEquivalentForUsage(&fit, weightReferenceModel, usageDetail{InputTokens: tc.tokens}, tc.tier, 272000)
		if !ok || math.Abs(got-tc.want) > 1e-8 {
			t.Errorf("%s quota equivalent=%g want=%g", tc.name, got, tc.want)
		}
	}
	cfg := defaultConfig()
	cfg.LearnedFit = &fit
	cfg.PricingMode = pricingModeCredits
	p := price{Model: weightReferenceModel}
	if got := calculateCost(p, usageDetail{InputTokens: 400000}, "fast", cfg); math.Abs(got-40*fit.FastLong.Value) > 1e-7 {
		t.Errorf("combined valuation=%g want=%g", got, 40*fit.FastLong.Value)
	}
	if got := calculateCost(p, usageDetail{InputTokens: 100000}, "fast", cfg); math.Abs(got-20) > 1e-8 {
		t.Errorf("standalone uncalibrated paid Fast must retain published 2x; got=%g", got)
	}
}

func TestReviewerCorrelatedModeTotalsDoNotProveACombinedRequestRate(t *testing.T) {
	var segments []quotaSegment
	for i := 0; i < 20; i++ {
		features := []segmentFeature{{Model: weightReferenceModel, Type: "input", Tokens: 1000000}}
		dp := 1.0
		if i%2 == 1 {
			features = []segmentFeature{{Model: weightReferenceModel, Type: "input", Tokens: 1000000, Fast: true}, {Model: weightReferenceModel, Type: "input", Tokens: 1000000, Long: true}, {Model: weightReferenceModel, Type: "input", Tokens: 1000000, Fast: true, Long: true}}
			// Fixed totals correlate perfectly even though three distinct modes occur.
			// Actual factors are F=3,L=1.2; the true combined mode is 3.6, not 4.3.
			dp = 3 + 1.2 + 3.6
		}
		segments = append(segments, quotaSegment{Account: "a", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 1900000000, EndAt: 1800000000 + int64(i), DP: dp, BoundaryWeight: 1, Features: features})
	}
	fit, err := fitQuotaWeights(segments, nil, 1800000100, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	if fit.FastLong != nil && appliedWeight(*fit.FastLong) {
		t.Fatalf("aggregate correlation invented a standalone combined request rate: %+v", fit.FastLong)
	}
}

func TestReviewerCustomModeDoesNotClaimLearnedModifiersAreApplied(t *testing.T) {
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeCustom
	cfg.CustomFastMultiplier = 3
	combined := weightEstimate{Source: "provisional", Applied: true, Value: 4, Low: 2, High: 8}
	cfg.LearnedFit = &weightFit{Available: true, Fast: weightEstimate{Source: "independent", Identified: true, Value: 2.2, Low: 2, High: 2.4}, LongContext: weightEstimate{Source: "independent", Identified: true, Value: 1.1, Low: 1, High: 1.2}, FastLong: &combined}
	p, _ := officialGPT6Price("gpt-6.1-sol")
	row := cfg.priceRow(p)
	if row.FastMultiplier != 3 || row.FastSource != "custom" || row.LongSource != "custom" || row.FastLong != nil || row.FastUncertaintyPercent != 0 || row.LongUncertaintyPercent != 0 {
		t.Fatalf("custom valuation reports unrelated learning as active: %+v", row)
	}
}
