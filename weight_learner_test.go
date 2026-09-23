package main

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

func TestWeightLearnerRecoversKnownSyntheticWeights(t *testing.T) {
	const now = int64(1_800_000_000)
	random := rand.New(rand.NewSource(20260923))
	models := []string{"gpt-5.6-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra"}
	types := []string{"input", "cache", "output"}
	segments := make([]quotaSegment, 1600)
	for i := range segments {
		account, window := "a", mainQuotaScope
		if i%2 == 1 {
			account, window = "b", weeklyQuotaScope
		}
		segment := quotaSegment{Account: account, Window: window, CycleID: int64(i/100 + 1),
			EndAt: now - int64(1600-i)*60, DP: 1, BoundaryWeight: 1, Features: []segmentFeature{}}
		for j := 0; j < 1+random.Intn(3); j++ {
			segment.Features = append(segment.Features, segmentFeature{
				Model: models[random.Intn(len(models))], Type: types[random.Intn(len(types))],
				Fast: random.Intn(2) == 1, Long: random.Intn(2) == 1,
				Tokens: int64(300_000 + random.Intn(1_700_000)),
			})
		}
		segments[i] = segment
	}
	opts := defaultWeightLearnerOptions()
	opts.CacheShareMinSD, opts.OutputShareMinSD, opts.TypeMinFisher = .001, .001, .001
	opts.ModelShareMinSD, opts.ModelMinFisher = .001, .001
	opts.CacheCycleRatioMinRange, opts.OutputCycleRatioMinRange = .001, .001
	model := newWeightModel(segments, nil, now, opts)
	truth := append([]float64(nil), model.priorMean...)
	for name, index := range model.index {
		if strings.HasPrefix(name, "scale:a|main|") {
			truth[index] = math.Log(.45)
		}
		if strings.HasPrefix(name, "scale:b|weekly|") {
			truth[index] = math.Log(.3)
		}
	}
	for name, value := range map[string]float64{
		"model:gpt-6-astra": 1.4, "model:gpt-6-sol": 1.1,
		"model:gpt-6-luna": 2.0, "model:gpt-5.6-terra": 1.3,
		"type:cache": .8, "type:output": 1.2,
		"fast": 2.2, "long": 1.4,
	} {
		index, ok := model.index[name]
		if !ok {
			t.Fatalf("missing parameter %s", name)
		}
		truth[index] = math.Log(value)
	}
	for i := range segments {
		segments[i].DP, _ = model.predict(segments[i], truth)
	}
	fit, err := fitQuotaWeights(segments, nil, now, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !fit.Available || fit.SegmentCount != len(segments) {
		t.Fatalf("fit = %#v", fit)
	}
	for name, want := range map[string]float64{
		"model:gpt-6-astra": 1.4, "model:gpt-6-sol": 1.1,
		"model:gpt-6-luna": 2, "model:gpt-5.6-terra": 1.3,
		"type:cache": .8, "type:output": 1.2, "fast": 2.2, "long": 1.4,
	} {
		var got float64
		for i, candidate := range fit.ParameterNames {
			if candidate == name {
				got = math.Exp(fit.LogParameters[i])
				break
			}
		}
		if math.Abs(got-want) > .08 {
			t.Errorf("%s = %.4f, want %.4f", name, got, want)
		}
	}
	if !fit.Fast.Identified || !fit.LongContext.Identified {
		t.Fatalf("synthetic factors should be identified: fast=%#v long=%#v", fit.Fast, fit.LongContext)
	}
}

func TestStableTokenCompositionLocksTypeRatiosAndTracksCycleDrift(t *testing.T) {
	const now = int64(1_800_000_000)
	scales := []float64{.2, .35, .25}
	var segments []quotaSegment
	for cycle, scale := range scales {
		for i := 0; i < 24; i++ {
			factor := int64(90 + i%5*5)
			segment := quotaSegment{Account: "a", Window: mainQuotaScope, CycleID: int64(cycle + 1), RegimeResetAt: int64(10_000 + cycle*1000),
				StartAt: now - 10_000 + int64(cycle*240+i*10), EndAt: now - 9_995 + int64(cycle*240+i*10), BoundaryWeight: 1,
				Features: []segmentFeature{
					{Model: "gpt-5.6-sol", Type: "input", Tokens: 9_000 * factor},
					{Model: "gpt-5.6-sol", Type: "cache", Tokens: 291_000 * factor},
					{Model: "gpt-5.6-sol", Type: "output", Tokens: 1_500 * factor},
				},
			}
			segment.DP = scale * referenceEquivalent(segment, pricingModeCredits, nil)
			segments = append(segments, segment)
		}
	}
	fit, err := fitQuotaWeights(segments, nil, now, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !fit.Available || len(fit.CycleScales) != 3 {
		t.Fatalf("fit=%#v", fit)
	}
	for _, diagnostic := range fit.TypeDiagnostics {
		if diagnostic.Unlocked {
			t.Fatalf("stable composition unlocked %s: %#v", diagnostic.Name, diagnostic)
		}
	}
	for i, cycle := range fit.CycleScales {
		if math.Abs(cycle.Scale.Value-scales[i]) > .04 {
			t.Errorf("cycle %d scale=%g want %g", i+1, cycle.Scale.Value, scales[i])
		}
	}
}
