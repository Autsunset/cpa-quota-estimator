package main

import (
	"math"
	"testing"
)

func pooledContrastSegments(count int) []quotaSegment {
	var segments []quotaSegment
	for i := 0; i < count; i++ {
		model, tokens, dp := weightReferenceModel, int64(2000000), 2.0
		var features []segmentFeature
		if i%2 == 0 {
			features = []segmentFeature{{Model: model, Type: "input", Tokens: tokens}}
		} else {
			// A fixed mixture supplies only one model-composition contrast.
			features = []segmentFeature{{Model: "gpt-6.1-sol", Type: "input", Tokens: 1000000}, {Model: "gpt-6.1-sol", Type: "cache", Tokens: 20000000}, {Model: "gpt-6.1-sol", Type: "output", Tokens: 200000}}
			dp = 1.5 * 1.4
		}
		segments = append(segments, quotaSegment{Account: "a", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 1900000000, EndAt: 1800000000 + int64(i), BoundaryWeight: 1, DP: dp, Features: features})
	}
	return segments
}

func TestFewContrastsRecoverPooledCompositionWithoutIndependentPrices(t *testing.T) {
	opts := defaultWeightLearnerOptions()
	opts.PooledDiagnosticOnly = true
	fit, err := fitQuotaWeights(pooledContrastSegments(12), nil, 1800000100, opts)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, pool := range fit.PooledModels {
		if pool.Model == "gpt-6.1-sol" {
			found = true
			if pool.Applied || !supportedWeight(pool.Estimate) || math.Abs(pool.Estimate.Value-1.4) > .15 {
				t.Fatalf("pool=%+v", pool)
			}
		}
	}
	if !found {
		t.Fatal("missing pooled estimate")
	}
	for _, row := range fit.Models {
		if row.Model == "gpt-6.1-sol" {
			if row.Input.Identified || row.Cache.Identified || row.Output.Identified {
				t.Fatal("pooled recovery falsely identified components")
			}
		}
	}
}

func TestApprovedPoolingIsAdditiveAndDoesNotExtrapolateUnseenComponents(t *testing.T) {
	segments := pooledContrastSegments(20)
	for i := range segments {
		if i%2 == 1 {
			segments[i].Features = segments[i].Features[:2]
			segments[i].DP = 1.4
		}
	}
	opts := defaultWeightLearnerOptions()
	opts.SkipPooledValidation = true
	opts.ApprovedPooledModels = map[string]bool{"gpt-6.1-sol": true}
	fit, err := fitQuotaWeights(segments, nil, 1800000100, opts)
	if err != nil {
		t.Fatal(err)
	}
	var model learnedModelWeights
	for _, row := range fit.Models {
		if row.Model == "gpt-6.1-sol" {
			model = row
		}
	}
	if model.Input.Source != "pooled" || model.Cache.Source != "pooled" || model.Output.Source != "prior" || model.Input.Identified {
		t.Fatalf("sources=%+v", model)
	}
	d1 := usageDetail{InputTokens: 1000000}
	d2 := usageDetail{InputTokens: 20000000, CacheReadTokens: 20000000}
	a, _ := learnedEquivalentForUsage(&fit, "gpt-6.1-sol", d1, "auto", 100000000)
	b, _ := learnedEquivalentForUsage(&fit, "gpt-6.1-sol", d2, "auto", 100000000)
	segment := segments[1]
	segment.Features[0].Long = false
	segment.Features[1].Long = false
	combined := learnedSegmentEquivalent(segment, fit)
	if math.Abs(a+b-combined) > 1e-9 {
		t.Fatalf("request sum=%g segment=%g", a+b, combined)
	}
	scale, _ := learnedScale(&fit, segment.Account, segment.Window, segment.CycleID, segment.RegimeResetAt)
	if math.Abs(fit.predict(segment, nil)-combined*scale) > 1e-5 {
		t.Fatal("stored pooled model disagrees with runtime")
	}
}

func TestOnlineCapacityStillAdaptsAfterLongStableHistory(t *testing.T) {
	state := onlineCycleScale{LogValue: math.Log(.5), Precision: 1}
	for i := 0; i < 1000; i++ {
		state.update(2, 1, 1, 0)
	}
	if state.Precision > onlineScaleMaxPrecision {
		t.Fatal("precision exceeded bound")
	}
	old := math.Exp(state.LogValue)
	for i := 0; i < 40; i++ {
		state.update(2, 2, 1, 0)
	}
	adapted := math.Exp(state.LogValue)
	if math.Abs(adapted-1) >= math.Abs(old-1)/3 {
		t.Fatalf("stale precision froze adaptation: old=%g adapted=%g precision=%g", old, adapted, state.Precision)
	}
}

func TestFixedMixedModesCannotPretendToIdentifyFastLongCombination(t *testing.T) {
	var segments []quotaSegment
	for i := 0; i < 30; i++ {
		baseline := int64(300000 + i%5*100000)
		features := []segmentFeature{{Model: weightReferenceModel, Type: "input", Tokens: baseline},
			{Model: weightReferenceModel, Type: "input", Tokens: 100000, Fast: true},
			{Model: weightReferenceModel, Type: "input", Tokens: 100000, Long: true},
			{Model: weightReferenceModel, Type: "input", Tokens: 100000, Fast: true, Long: true}}
		segments = append(segments, quotaSegment{Account: "a", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 1900000000, EndAt: 1800000000 + int64(i), DP: float64(baseline)/1e6 + .3 + .15 + .45, BoundaryWeight: 1, Features: features})
	}
	fit, err := fitQuotaWeights(segments, nil, 1800000100, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	if fit.FastLong != nil {
		t.Fatalf("fixed aggregate mixture was mistaken for co-exposed mode: %+v", fit.FastLong)
	}
	if fit.Fast.Identified || fit.LongContext.Identified {
		t.Fatal("fixed mixed modes must not identify standalone coefficients")
	}
}
