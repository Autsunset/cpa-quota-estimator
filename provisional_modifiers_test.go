package main

import (
	"math"
	"testing"
)

func separateModifierSegments(count int) []quotaSegment {
	var result []quotaSegment
	for i := 0; i < count; i++ {
		feature := segmentFeature{Model: weightReferenceModel, Type: "input", Tokens: 100000}
		dp := .1
		if i%3 == 1 {
			feature.Fast = true
			dp *= 2.8
		}
		if i%3 == 2 {
			feature.Long = true
			feature.Tokens = 400000
			dp = .4 * 1.1
		}
		result = append(result, quotaSegment{Account: "a", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 1900000000, EndAt: 1800000000 + int64(i), BoundaryWeight: 1, DP: dp, Features: []segmentFeature{feature}})
	}
	return result
}

func TestWeakModifierEvidenceUsesExplicitPriorShrinkageAndGrowsSmoothly(t *testing.T) {
	var first, last weightFit
	for _, n := range []int{9, 90, 900} {
		fit, err := fitQuotaWeights(separateModifierSegments(n), nil, 1800001000, defaultWeightLearnerOptions())
		if err != nil {
			t.Fatal(err)
		}
		if fit.FastLong != nil {
			t.Fatal("real separate contrasts must not create redundant combined parameter")
		}
		if !appliedWeight(fit.Fast) || !appliedWeight(fit.LongContext) {
			t.Fatalf("genuine weak evidence discarded at n=%d: fast=%+v long=%+v", n, fit.Fast, fit.LongContext)
		}
		if fit.Fast.Value < 2.5-1e-6 || fit.Fast.Value > 2.8+.02 || fit.LongContext.Value < 1-.01 || fit.LongContext.Value > 1.1+.02 {
			t.Fatalf("prior shrinkage overshot n=%d", n)
		}
		if n == 9 {
			first = fit
			if fit.Fast.Source != "provisional" || fit.Fast.Identified || fit.Fast.PriorLocked || fit.Fast.High <= fit.Fast.Low {
				t.Fatal("weak posterior falsely certified or lacks interval")
			}
		}
		last = fit
	}
	if math.Abs(last.Fast.Value-2.8) >= math.Abs(first.Fast.Value-2.8) {
		t.Fatal("more genuine contrast did not strengthen estimate")
	}
}

func TestAllFastWithoutWithinCycleContrastRetainsExactQuotaPrior(t *testing.T) {
	segments := separateModifierSegments(30)
	for i := range segments {
		segments[i].Features[0].Fast = true
		segments[i].Features[0].Long = false
		segments[i].Features[0].Tokens = 100000
		segments[i].DP = .3
	}
	fit, err := fitQuotaWeights(segments, nil, 1800001000, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	if fit.Fast.Source != "prior" || fit.Fast.Value != 2.5 || fit.Fast.Identified || fit.FastLong != nil {
		t.Fatalf("all-Fast data invented a contrast: %+v", fit.Fast)
	}
}
