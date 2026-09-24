package main

import (
	"math"
	"testing"
)

func TestOnlineScaleBorrowsPreviousCycleBeforeFirstCrossing(t *testing.T) {
	tracker := &onlineScaleTracker{ByCycle: map[string]*onlineCycleScale{}, LastByGroup: map[string]*onlineCycleScale{}, RandomWalkSigma: .35}
	previous := quotaSegment{Account: "test", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 1000}
	prior := tracker.forSegment(previous)
	prior.LogValue = math.Log(.2) // 500 Credits per quota percent.
	prior.Precision = 400
	current := quotaSegment{Account: "test", Window: mainQuotaScope, CycleID: 2, RegimeResetAt: 2000}
	state := tracker.forSegment(current)
	if math.Abs(100/math.Exp(state.LogValue)-500) > 1e-8 {
		t.Fatalf("unobserved cycle lost previous scale: %#v", state)
	}
	wantPrecision := 1 / (1/400.0 + .35*.35)
	if math.Abs(state.Precision-wantPrecision) > 1e-8 {
		t.Fatalf("random-walk uncertainty = %f want %f", state.Precision, wantPrecision)
	}
	// The first crossing is noisy: the direct adjacent-sample capacity says
	// 200 Credits/%, while the preceding cycle and later data support 500.
	points := []quotaPoint{{Time: 100, UsedPercent: 0, WindowCostUSD: 0}, {Time: 200, UsedPercent: 1, WindowCostUSD: 200}}
	legacy := estimateCapacity(points)
	state.update(.5, 1, 1, 0)
	online := 100 / math.Exp(state.LogValue)
	if !legacy.Available || math.Abs(legacy.FullWindowCostUSD/100-200) > 1e-8 {
		t.Fatalf("legacy estimate=%#v", legacy)
	}
	if math.Abs(online-500) >= math.Abs(legacy.FullWindowCostUSD/100-500) {
		t.Fatalf("online=%f legacy=%f", online, legacy.FullWindowCostUSD/100)
	}
}
