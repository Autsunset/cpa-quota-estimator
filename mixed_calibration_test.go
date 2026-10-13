package main

import (
	"context"
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

func mixedCalibrationSegments(count int, reversedTail bool) []quotaSegment {
	random := rand.New(rand.NewSource(20261013))
	result := make([]quotaSegment, 0, count)
	for i := 0; i < count; i++ {
		features := []segmentFeature{{Model: "gpt-6-astra", Type: "input", Tokens: 2000000}}
		dp := 1.6
		if i%2 == 1 {
			input := int64(300000 + random.Intn(1700000))
			input -= input % 50
			cache := int64(1000000 + random.Intn(24000000))
			factor := 1.8
			if reversedTail && i >= count*2/3 {
				factor, cache = 1, 100000
			}
			features = []segmentFeature{
				{Model: legacyWeightReferenceModel, Type: "input", Tokens: input},
				{Model: legacyWeightReferenceModel, Type: "cache", Tokens: cache},
				{Model: legacyWeightReferenceModel, Type: "output", Tokens: input / 50},
			}
			dp = .8 * (factor*.44*float64(input) + .05*float64(cache)) / 1000000
		}
		result = append(result, quotaSegment{Account: "mixed", Window: mainQuotaScope, CycleID: 1,
			RegimeResetAt: 1900000000, EndAt: 1800000000 + int64(i), BoundaryWeight: 1, DP: dp, Features: features})
	}
	return result
}

func TestMixedCalibrationKeepsCacheIndependentAndPoolsInputOutput(t *testing.T) {
	opts := defaultWeightLearnerOptions()
	opts.ReferenceModel = "gpt-6-astra"
	fit, err := fitQuotaWeights(mixedCalibrationSegments(180, false), nil, 1800001000, opts)
	if err != nil || !fit.Available {
		t.Fatalf("fit unavailable: %v", err)
	}
	var target learnedModelWeights
	for _, row := range fit.Models {
		if row.Model == legacyWeightReferenceModel {
			target = row
		}
	}
	if !supportedWeight(target.Cache) || math.Abs(target.Cache.Value/.05-1) > .08 {
		t.Fatalf("cache must retain its own independently fitted coefficient: %+v", target)
	}
	if target.Input.Source != "joint" || target.Output.Source != "joint" ||
		target.Input.Identified || target.Output.Identified ||
		math.Abs(target.Input.Value/(.4*1.8)-1) > .08 || math.Abs(target.Output.Value/(2*1.8)-1) > .08 {
		t.Fatalf("unseparated input/output must use a validated joint estimate: %+v", target)
	}
	var applied bool
	for _, pool := range fit.PooledModels {
		if pool.Model == legacyWeightReferenceModel {
			applied = pool.Applied && pool.Evidence.Accepted && pool.Evidence.ValidationSegments >= 4
			if pool.Scope != "residual" || len(pool.TokenTypes) != 2 ||
				pool.TokenTypes[0] != "input" || pool.TokenTypes[1] != "output" || pool.coversTokenType("cache") {
				t.Fatalf("joint coefficient scope includes independently calibrated cache: %+v", pool)
			}
		}
	}
	if !applied {
		t.Fatalf("missing validated residual multiplier: %+v", fit.PooledModels)
	}
	cfg := defaultConfig()
	cfg.LearnedFit = &fit
	p, _ := officialGPT6Price(legacyWeightReferenceModel)
	row := cfg.priceRow(p)
	for field, want := range map[string]float64{"input": 180, "cache_read": 12.5, "output": 900} {
		if math.Abs(componentPrice(row.Calculated, field)/want-1) > .08 {
			t.Fatalf("%s price did not use exactly one coefficient: %+v", field, row)
		}
	}
	if !row.ComponentAdjustments["cache_read"].Calibrated || row.ComponentAdjustments["input"].Calibrated ||
		row.ComponentAdjustments["output"].Calibrated || row.ComponentAdjustments["cache_read"].Source != "independent" ||
		row.ComponentAdjustments["input"].Source != "joint" {
		t.Fatalf("mixed evidence was mislabeled: %+v", row.ComponentAdjustments)
	}
	for _, segment := range mixedCalibrationSegments(180, false) {
		scale, _ := learnedScale(&fit, segment.Account, segment.Window, segment.CycleID, segment.RegimeResetAt)
		if math.Abs(fit.predict(segment, nil)-learnedSegmentEquivalent(segment, fit)*scale) > 1e-5 {
			t.Fatal("serialized prediction disagrees with mixed runtime allocation")
		}
	}
}

func TestMixedValidationCountsOnlyJointComponentExposures(t *testing.T) {
	segments := mixedCalibrationSegments(120, false)
	for i := 0; i < 100; i++ {
		cache := int64(1000000 + i*100000)
		segments = append(segments, quotaSegment{Account: "mixed", Window: mainQuotaScope, CycleID: 1,
			RegimeResetAt: 1900000000, EndAt: 1800000200 + int64(i), BoundaryWeight: 1,
			DP:       .8 * .05 * float64(cache) / 1000000,
			Features: []segmentFeature{{Model: legacyWeightReferenceModel, Type: "cache", Tokens: cache}}})
	}
	opts := defaultWeightLearnerOptions()
	opts.ReferenceModel = "gpt-6-astra"
	fit, err := fitQuotaWeights(segments, nil, 1800001000, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, pool := range fit.PooledModels {
		if pool.Model == legacyWeightReferenceModel {
			if !pool.Applied || pool.Evidence.ValidationSegments != 20 {
				t.Fatalf("cache-only observations padded joint validation or moved its cutoff: %+v", pool)
			}
			return
		}
	}
	t.Fatal("joint candidate missing")
}

func TestJointValidationRejectsChangedTrainingScope(t *testing.T) {
	expected := pooledModelEstimate{Model: "gpt-6.1-sol", Scope: "residual", TokenTypes: []string{"input", "output"}}
	evidence := pooledValidation(expected, pooledContrastSegments(36), nil, legacyWeightLearnerOptions())
	if evidence.Accepted || evidence.Reason != "training_scope_changed" {
		t.Fatalf("a whole-model training coefficient certified a different residual group: %+v", evidence)
	}
}

func TestMixedCalibrationDoesNotApplyEvidenceToUnseenOutput(t *testing.T) {
	segments := mixedCalibrationSegments(180, false)
	for i := range segments {
		if len(segments[i].Features) == 3 {
			segments[i].Features = segments[i].Features[:2]
		}
	}
	opts := defaultWeightLearnerOptions()
	opts.ReferenceModel = "gpt-6-astra"
	fit, err := fitQuotaWeights(segments, nil, 1800001000, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range fit.Models {
		if row.Model == legacyWeightReferenceModel {
			if row.Output.Source != "prior" || row.Output.Value != 2 || row.Output.Identified {
				t.Fatalf("unseen output borrowed other components' evidence: %+v", row)
			}
		}
	}
	for _, pool := range fit.PooledModels {
		if pool.Model == legacyWeightReferenceModel && pool.coversTokenType("output") {
			t.Fatalf("unseen output became part of the fitted scope: %+v", pool)
		}
	}
}

func TestReferenceCanJointlyCalibrateCacheOutputWithoutMovingInput(t *testing.T) {
	random := rand.New(rand.NewSource(20261014))
	var segments []quotaSegment
	for i := 0; i < 180; i++ {
		input := int64(300000 + random.Intn(2000000))
		cache := int64(1000000 + random.Intn(20000000))
		cache -= cache % 1000
		segments = append(segments, quotaSegment{Account: "reference", Window: mainQuotaScope, CycleID: 1,
			RegimeResetAt: 1900000000, EndAt: 1800000000 + int64(i), BoundaryWeight: 1,
			DP: .7 * (float64(input) + .105*1.6*float64(cache)) / 1000000,
			Features: []segmentFeature{{Model: "gpt-6-astra", Type: "input", Tokens: input},
				{Model: "gpt-6-astra", Type: "cache", Tokens: cache},
				{Model: "gpt-6-astra", Type: "output", Tokens: cache / 1000}}})
	}
	fit, err := fitQuotaWeights(segments, nil, 1800001000, defaultWeightLearnerOptions())
	if err != nil || !fit.Available {
		t.Fatalf("fit unavailable: %v", err)
	}
	for _, row := range fit.Models {
		if row.Model == "gpt-6-astra" {
			if row.Input.Source != "anchor" || row.Input.Value != 1 || row.Input.Low != 1 || row.Input.High != 1 ||
				row.Cache.Source != "joint" || row.Output.Source != "joint" ||
				math.Abs(row.Cache.Value/.16-1) > .08 || math.Abs(row.Output.Value/8-1) > .08 {
				t.Fatalf("joint reference evidence moved its input gauge or was discarded: %+v", row)
			}
		}
	}
	for _, pool := range fit.PooledModels {
		if pool.Model == "gpt-6-astra" && pool.coversTokenType("input") {
			t.Fatal("reference input was included in a joint coefficient")
		}
	}
}

func TestMixedFormulaInvalidatesOldOnlineScaleEvenWithSameReference(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "mixed-basis.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	old := weightFit{Available: true, EligibilityVersion: 7, ReferenceModel: "gpt-6-astra",
		ReferenceInputCredits: 250, FittedAt: 100,
		CycleScales: []learnedCycleScale{{Account: "mixed", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 200,
			Scale: weightEstimate{Value: .8, Low: .7, High: .9}}}}
	if err = s.seedOnlineCycleScales(ctx, &old); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE online_cycle_scales SET log_scale=?,updated_at=1000`, math.Log(8)); err != nil {
		t.Fatal(err)
	}
	current := old
	current.EligibilityVersion = weightEligibilityVersion
	current.FittedAt = 200
	current.CycleScales = []learnedCycleScale{{Account: "mixed", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 200,
		Scale: weightEstimate{Value: .9, Low: .8, High: 1}}}
	if old.weightBasis() == current.weightBasis() {
		t.Fatal("a formula change reused a scale learned under different active coefficients")
	}
	if err = s.seedOnlineCycleScales(ctx, &current); err != nil {
		t.Fatal(err)
	}
	overlay, err := s.overlayOnlineCycleScales(ctx, current)
	if err != nil || math.Abs(overlay.CycleScales[0].Scale.Value-.9) > 1e-9 {
		t.Fatalf("recent old-formula scale survived the migration: %+v err=%v", overlay, err)
	}
}

func TestJointPriceAllocationRequiresAcceptedMatchingScope(t *testing.T) {
	cfg := defaultConfig()
	cfg.LearnedFit = &weightFit{Available: true, ReferenceModel: "gpt-6-astra", ReferenceInputCredits: 250,
		Models: []learnedModelWeights{{Model: legacyWeightReferenceModel,
			Input:  weightEstimate{Source: "joint", Value: .72, Low: .6, High: .8, Identified: true},
			Cache:  weightEstimate{Source: "independent", Value: .05, Low: .04, High: .06, Identified: true},
			Output: weightEstimate{Source: "joint", Value: 3.6, Low: 3, High: 4, Identified: true}}},
		PooledModels: []pooledModelEstimate{{Model: legacyWeightReferenceModel, Scope: "residual",
			TokenTypes: []string{"input", "output"}, Applied: false,
			Composition: map[string]pooledComponentSupport{"input": {Tokens: 100}, "cache": {Tokens: 100}, "output": {Tokens: 10}}}}}
	p, _ := officialGPT6Price(legacyWeightReferenceModel)
	row := cfg.priceRow(p)
	if row.Calculated.Input != 100 || row.Calculated.Output != 500 || math.Abs(row.Calculated.CacheRead-12.5) > 1e-9 {
		t.Fatalf("raw identified flags bypassed joint validation: %+v", row)
	}
	cfg.LearnedFit.PooledModels[0].Applied = true
	cfg.LearnedFit.PooledModels[0].TokenTypes = []string{"output"}
	row = cfg.priceRow(p)
	if row.Calculated.Input != 100 || math.Abs(row.Calculated.Output-900) > 1e-9 ||
		row.ComponentAdjustments["output"].Calibrated || row.ComponentAdjustments["input"].Source != "prior" {
		t.Fatalf("allocated joint evidence escaped its explicit component scope: %+v", row)
	}
}

func TestMixedCalibrationRuntimeKeepsSpeedAndContextModifiersSeparate(t *testing.T) {
	segments := mixedCalibrationSegments(240, false)
	for i := range segments {
		fast, long := i%5 < 2, i%7 < 3
		for j := range segments[i].Features {
			segments[i].Features[j].Fast, segments[i].Features[j].Long = fast, long
		}
		if fast {
			segments[i].DP *= 3
		}
		if long {
			segments[i].DP *= 1.2
		}
	}
	fit, err := fitQuotaWeights(segments, nil, 1800001000, defaultWeightLearnerOptions())
	if err != nil || !fit.Available {
		t.Fatalf("fit unavailable: %v", err)
	}
	if !supportedWeight(fit.Fast) || !supportedWeight(fit.LongContext) {
		t.Fatalf("joint nuisance lost independent mode contrasts: fast=%+v long=%+v", fit.Fast, fit.LongContext)
	}
	for _, row := range fit.Models {
		if row.Model == legacyWeightReferenceModel && (row.Input.Source != "joint" || !supportedWeight(row.Cache)) {
			t.Fatalf("mixed calibration disappeared when independent modes were added: %+v", row)
		}
	}
	for _, segment := range segments {
		first := segment.Features[0]
		var detail usageDetail
		for _, feature := range segment.Features {
			switch feature.Type {
			case "input":
				detail.InputTokens += feature.Tokens
			case "cache":
				detail.InputTokens += feature.Tokens
				detail.CacheReadTokens += feature.Tokens
			case "output":
				detail.OutputTokens += feature.Tokens
			}
		}
		tier, threshold := "", int64(100000000)
		if first.Fast {
			tier = "fast"
		}
		if first.Long {
			threshold = 1
		}
		request, ok := learnedEquivalentForUsage(&fit, first.Model, detail, tier, threshold)
		combined := learnedSegmentEquivalent(segment, fit)
		if !ok || math.Abs(request-combined) > 1e-9 {
			t.Fatalf("mode flags multiplied mixed components inconsistently: request=%g segment=%g", request, combined)
		}
		scale, _ := learnedScale(&fit, segment.Account, segment.Window, segment.CycleID, segment.RegimeResetAt)
		if math.Abs(fit.predict(segment, nil)-combined*scale) > 1e-5 {
			t.Fatal("mixed mode live estimates differ from serialized fit predictions")
		}
	}
}

func TestMixedCalibrationValidationFailureKeepsUnidentifiedPartsAtPrior(t *testing.T) {
	opts := defaultWeightLearnerOptions()
	opts.ReferenceModel = "gpt-6-astra"
	fit, err := fitQuotaWeights(mixedCalibrationSegments(180, true), nil, 1800001000, opts)
	if err != nil || !fit.Available {
		t.Fatalf("fit unavailable: %v", err)
	}
	found := false
	for _, pool := range fit.PooledModels {
		if pool.Model == legacyWeightReferenceModel {
			found = true
			if pool.Applied || pool.Evidence.Accepted {
				t.Fatalf("a joint coefficient that fails later observations was applied: %+v", pool)
			}
		}
	}
	if !found {
		t.Fatal("a partially identified model must still expose its joint candidate")
	}
	for _, row := range fit.Models {
		if row.Model == legacyWeightReferenceModel {
			if row.Input.Source != "prior" || row.Output.Source != "prior" || row.Input.Value != .4 || row.Output.Value != 2 {
				t.Fatalf("rejected joint evidence leaked into active rates: %+v", row)
			}
		}
	}
}
