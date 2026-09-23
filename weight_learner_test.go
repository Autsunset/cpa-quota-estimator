package main

import (
	"math"
	"math/rand"
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
	model := newWeightModel(segments, nil)
	truth := append([]float64(nil), model.priorMean...)
	for name, value := range map[string]float64{
		"scale:a|main": .45, "scale:b|weekly": .3,
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
	fit, err := fitQuotaWeights(segments, nil, now, defaultWeightLearnerOptions())
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
