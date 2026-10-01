package main

import (
	"math"
	"math/rand"
	"testing"
)

func componentSyntheticSegments(count int, stable bool) []quotaSegment {
	const now = int64(1800000000)
	random := rand.New(rand.NewSource(20261001))
	var segments []quotaSegment
	for i := 0; i < count; i++ {
		input := int64(200000 + random.Intn(2800000))
		cache := int64(1000000 + random.Intn(90000000))
		output := int64(20000 + random.Intn(500000))
		baseline := int64(300000 + random.Intn(1500000))
		if stable {
			input = 100000
			cache = 10000000
			output = 20000
			multiplier := int64(1 + i%5)
			input *= multiplier
			cache *= multiplier
			output *= multiplier
		}
		segment := quotaSegment{Account: "a", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: now + 86400, EndAt: now - int64(count-i)*30, BoundaryWeight: 1,
			Features: []segmentFeature{{Model: weightReferenceModel, Type: "input", Tokens: baseline}, {Model: "gpt-6.1-sol", Type: "input", Tokens: input}, {Model: "gpt-6.1-sol", Type: "cache", Tokens: cache}, {Model: "gpt-6.1-sol", Type: "output", Tokens: output}}}
		segment.DP = .7 * (float64(baseline) + .5*1.1*float64(input) + .025*.65*float64(cache) + 2.5*1.5*float64(output)) / 1_000_000
		segments = append(segments, segment)
	}
	return segments
}

func TestComponentCalibrationRecoversDifferentInputCacheAndOutput(t *testing.T) {
	fit, err := fitQuotaWeights(componentSyntheticSegments(300, false), nil, 1800000000, defaultWeightLearnerOptions())
	if err != nil || !fit.Available {
		t.Fatalf("fit=%#v err=%v", fit, err)
	}
	for _, row := range fit.Models {
		if row.Model != "gpt-6.1-sol" {
			continue
		}
		for _, check := range []struct {
			estimate weightEstimate
			want     float64
		}{{row.Input, .55}, {row.Cache, .01625}, {row.Output, 3.75}} {
			if check.estimate.PriorLocked || !check.estimate.Identified || math.Abs(check.estimate.Value/check.want-1) > .08 {
				t.Fatalf("independent weight=%#v want %g", check.estimate, check.want)
			}
		}
		return
	}
	t.Fatal("missing GPT-6.1 Sol")
}

func TestStableCompositionCannotPretendToCalibrateSeparateRates(t *testing.T) {
	fit, err := fitQuotaWeights(componentSyntheticSegments(300, true), nil, 1800000000, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range fit.Models {
		if row.Model != "gpt-6.1-sol" {
			continue
		}
		if !row.Input.PriorLocked || !row.Cache.PriorLocked || !row.Output.PriorLocked || row.Input.Value != .5 || row.Cache.Value != .025 || row.Output.Value != 2.5 {
			t.Fatalf("proportional components were falsely identified: %#v", row)
		}
		return
	}
	t.Fatal("missing GPT-6.1 Sol")
}

func TestCacheOnlyCalibrationDoesNotChangeInputOrOutputPrices(t *testing.T) {
	cfg := defaultConfig()
	cfg.LearnedFit = &weightFit{Available: true, Models: []learnedModelWeights{{Model: "gpt-6.1-sol", Input: weightEstimate{Value: .9, PriorLocked: true}, Cache: weightEstimate{Value: .04, Low: .035, High: .045, Identified: true}, Output: weightEstimate{Value: 4, PriorLocked: true}}}}
	p, _ := officialGPT6Price("gpt-6.1-sol")
	for _, mode := range []string{pricingModeAPI, pricingModeCredits} {
		cfg.PricingMode = mode
		official := priceForPricingMode(p, mode)
		row := cfg.priceRow(p)
		if row.Calculated.Input != official.Input || row.Calculated.Output != official.Output || row.Calculated.CacheWrite != official.CacheWrite || math.Abs(row.Calculated.CacheRead/official.CacheRead-1.6) > 1e-9 {
			t.Fatalf("cache adjustment leaked into another field: %#v", row)
		}
		if row.ComponentAdjustments["input"].Calibrated || !row.ComponentAdjustments["cache_read"].Calibrated || row.ComponentAdjustments["output"].Calibrated {
			t.Fatalf("incorrect component statuses: %#v", row.ComponentAdjustments)
		}
	}
	cfg.PricingMode = pricingModeCredits
	cfg.PriceCatalog = map[string]price{p.Model: p}
	previous := *cfg.LearnedFit
	previous.Models = append([]learnedModelWeights(nil), cfg.LearnedFit.Models...)
	previous.Models[0].Cache.Value = .025
	if !fitRepricingDue(&previous, cfg, 0, 1) {
		t.Fatal("a cache-only rate change must schedule repricing")
	}
}

func TestCacheSamplesUnlockOnlyTheCacheParameter(t *testing.T) {
	segments := componentSyntheticSegments(150, false)
	for i := range segments {
		baseline := segments[i].Features[0].Tokens
		cache := segments[i].Features[2].Tokens
		segments[i].Features = []segmentFeature{{Model: weightReferenceModel, Type: "input", Tokens: baseline}, {Model: "gpt-6.1-sol", Type: "cache", Tokens: cache}}
		segments[i].DP = .7 * (float64(baseline) + .04*float64(cache)) / 1_000_000
	}
	fit, err := fitQuotaWeights(segments, nil, 1800000000, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range fit.Models {
		if row.Model != "gpt-6.1-sol" {
			continue
		}
		if !row.Input.PriorLocked || !row.Output.PriorLocked || row.Cache.PriorLocked || !row.Cache.Identified || math.Abs(row.Cache.Value/.04-1) > .08 {
			t.Fatalf("cache-only samples must not identify input/output: %#v", row)
		}
		return
	}
	t.Fatal("missing GPT-6.1 Sol")
}

func TestComponentFormulaMigrationBypassesOrdinaryFitRateLimitOnce(t *testing.T) {
	previous := testAnchorFit()
	previous.EligibilityVersion = 1
	current := *previous
	current.EligibilityVersion = weightEligibilityVersion
	cfg := defaultConfig()
	cfg.LearnedFit = &current
	const now = int64(1000000)
	if !fitRepricingDue(previous, cfg, now-1, now) {
		t.Fatal("formula upgrade must reprice old shared-multiplier history immediately")
	}
	if !fitRepricingDue(nil, cfg, now-1, now) {
		t.Fatal("migration without an active compatible fit must finish repricing after the new fit")
	}
	if fitRepricingDue(&current, cfg, now-1, now) {
		t.Fatal("a current formula must respect the ordinary six-hour rate limit")
	}
	cfg.PricingMode = pricingModeCustom
	if fitRepricingDue(previous, cfg, now-1, now) {
		t.Fatal("a formula migration must not introduce learner pricing in custom mode")
	}
}
