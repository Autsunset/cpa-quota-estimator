package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
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

func TestSegmentValueUsesActualCustomCostIncludingWritesAndThreshold(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "segments.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = seedPrices(ctx, s); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeCustom
	cfg.CustomLongThreshold = 100_000
	cfg.CustomLongContext = true
	zero, one := float64(0), float64(1)
	first := event{Account: "a", Model: "gpt-5.6-sol", RequestedAt: 100, ObservedAt: 100, UsedPercent: &zero, ResetAt: 1000, WindowMinutes: 15}
	if err = s.insertEvent(ctx, first, time.Minute); err != nil {
		t.Fatal(err)
	}
	second := event{Account: "a", Model: "gpt-5.6-sol", RequestedAt: 200, ObservedAt: 200, UsedPercent: &one, ResetAt: 1000, WindowMinutes: 15, InputTokens: 150_000, CacheWriteTokens: 10_000, TotalTokens: 150_000}
	p, _, err := s.getPrice(ctx, "gpt-5.6-sol")
	if err != nil {
		t.Fatal(err)
	}
	second.CostUSD = calculateCost(p, usageDetail{InputTokens: second.InputTokens, CacheCreationTokens: second.CacheWriteTokens}, "", cfg)
	if err = s.insertEvent(ctx, second, time.Minute); err != nil {
		t.Fatal(err)
	}
	segments, err := s.quotaSegments(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) == 0 {
		t.Fatal("missing crossing segment")
	}
	prefix, err := s.segmentCostPrefix(ctx, segments[0])
	if err != nil {
		t.Fatal(err)
	}
	got := prefix.value(segments[0].FeatureStartEventID, segments[0].FeatureEndEventID)
	if math.Abs(got-second.CostUSD) > 1e-9 {
		t.Fatalf("segment selected value=%f request cost=%f", got, second.CostUSD)
	}
	if got <= .6 {
		t.Fatalf("custom long/cache-write cost was lost: %f", got)
	}
}
