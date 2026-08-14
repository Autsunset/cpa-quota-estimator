package main

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

func TestPublicReleaseDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HistoryDays != 365 {
		t.Fatalf("history days = %d, want 365", cfg.HistoryDays)
	}
	for _, marker := range [][]byte{[]byte("bridgeState"), []byte("directApi"), []byte("CPA Management Key"), []byte("renderPeriods"), []byte("reset_at")} {
		if !bytes.Contains(dashboardHTML, marker) {
			t.Fatalf("dashboard is missing public-host fallback marker %q", marker)
		}
	}
}

func TestEstimateCapacity(t *testing.T) {
	points := []quotaPoint{
		{Time: 1000, UsedPercent: 10, ResetAt: 9000, WindowTokens: 100_000_000, WindowCostUSD: 100},
		{Time: 2000, UsedPercent: 12, ResetAt: 9000, WindowTokens: 160_000_000, WindowCostUSD: 160},
		{Time: 3000, UsedPercent: 15, ResetAt: 9000, WindowTokens: 250_000_000, WindowCostUSD: 250},
	}
	e := estimateCapacity(points)
	if !e.Available || e.SampleCount != 2 || e.PercentSpan != 5 {
		t.Fatalf("unexpected estimate: %#v", e)
	}
	if e.FullWindowTokens != 3_000_000_000 || e.FullWindowCostUSD != 3000 {
		t.Fatalf("capacity = %.0f tokens, %.2f USD", e.FullWindowTokens, e.FullWindowCostUSD)
	}
	if e.RemainingTokens != 2_550_000_000 || e.RemainingCostUSD != 2550 {
		t.Fatalf("remaining = %.0f tokens, %.2f USD", e.RemainingTokens, e.RemainingCostUSD)
	}
}

func TestEstimateCapacityUsesFirstMonotonicCrossings(t *testing.T) {
	points := []quotaPoint{
		{Time: 1000, UsedPercent: 10, ResetAt: 9000, WindowTokens: 100, WindowCostUSD: 10},
		{Time: 1100, UsedPercent: 10, ResetAt: 9000, WindowTokens: 180, WindowCostUSD: 18},
		{Time: 1200, UsedPercent: 11, ResetAt: 9000, WindowTokens: 200, WindowCostUSD: 20},
		{Time: 1300, UsedPercent: 10, ResetAt: 9000, WindowTokens: 230, WindowCostUSD: 23}, // stale concurrent response
		{Time: 1400, UsedPercent: 11, ResetAt: 9000, WindowTokens: 280, WindowCostUSD: 28},
		{Time: 1500, UsedPercent: 12, ResetAt: 9000, WindowTokens: 300, WindowCostUSD: 30},
	}
	e := estimateCapacity(points)
	if e.SampleCount != 2 || e.FullWindowTokens != 10_000 || e.FullWindowCostUSD != 1000 {
		t.Fatalf("monotonic estimate = %#v", e)
	}
	history := capacityHistory(points)
	if len(history) != 2 || history[0].FullWindowTokens != 10_000 || history[1].FullWindowCostUSD != 1000 {
		t.Fatalf("capacity history = %#v", history)
	}
}

func TestEstimateBurnFastPace(t *testing.T) {
	const week = int64(7 * 24 * 60 * 60)
	points := []quotaPoint{{Time: week / 2, UsedPercent: 70, ResetAt: week, WindowMinutes: week / 60}}
	got := estimateBurn(points, week/2)
	if !got.Available || got.Status != "fast" || !got.WillExhaustBeforeReset {
		t.Fatalf("unexpected burn forecast: %#v", got)
	}
	if got.TimeProgressPercent != 50 || got.ExpectedUsedPercent != 50 || got.PaceDeltaPercent != 20 || got.PaceRatio != 1.4 {
		t.Fatalf("unexpected pace: %#v", got)
	}
	if math.Abs(got.AveragePercentPerDay-20) > 1e-9 || math.Abs(got.SustainablePercentPerDay-100.0/7) > 1e-9 {
		t.Fatalf("unexpected daily rate: %#v", got)
	}
	if got.ProjectedUsedAtReset != 140 || got.EstimatedExhaustAt != 5*24*60*60 {
		t.Fatalf("unexpected projection: %#v", got)
	}
}

func TestEstimateBurnSlowPaceLastsToReset(t *testing.T) {
	const week = int64(7 * 24 * 60 * 60)
	points := []quotaPoint{{Time: week / 2, UsedPercent: 25, ResetAt: week, WindowMinutes: week / 60}}
	got := estimateBurn(points, week/2)
	if !got.Available || got.Status != "slow" || got.WillExhaustBeforeReset {
		t.Fatalf("unexpected burn forecast: %#v", got)
	}
	if got.ProjectedUsedAtReset != 50 || got.EstimatedExhaustAt != 14*24*60*60 {
		t.Fatalf("unexpected projection: %#v", got)
	}
}

func TestEstimateBurnRecentPace(t *testing.T) {
	const day = int64(24 * 60 * 60)
	const week = 7 * day
	points := []quotaPoint{
		{Time: day, UsedPercent: 10, ResetAt: week, WindowMinutes: week / 60},
		{Time: 2 * day, UsedPercent: 30, ResetAt: week, WindowMinutes: week / 60},
		{Time: 3 * day, UsedPercent: 40, ResetAt: week, WindowMinutes: week / 60},
	}
	got := estimateBurn(points, 3*day)
	if !got.RecentAvailable || got.RecentWindowSeconds != day || got.RecentPercentSpan != 10 {
		t.Fatalf("unexpected recent sample: %#v", got)
	}
	if got.RecentPercentPerDay != 10 || got.RecentProjectedAtReset != 80 {
		t.Fatalf("unexpected recent pace: %#v", got)
	}
	if got.RecentEstimatedExhaustAt != 9*day || got.RecentWillExhaustBefore {
		t.Fatalf("unexpected recent exhaustion: %#v", got)
	}
}

func TestCalculateCostCacheLongAndFast(t *testing.T) {
	p := price{Input: 5, Output: 30, CacheRead: .5, CacheWrite: 6.25, LongInput: 10, LongOutput: 45, LongRead: 1, LongWrite: 12.5, FastInput: 10, FastOutput: 60, FastRead: 1, FastWrite: 12.5}
	cfg := defaultConfig()
	detail := usageDetail{InputTokens: 300_000, OutputTokens: 10_000, CacheReadTokens: 200_000, CacheCreationTokens: 20_000}
	got := calculateCost(p, detail, "priority", cfg)
	want := (80_000.0*10 + 200_000.0*1 + 20_000.0*12.5 + 10_000.0*45) / 1_000_000 * 2.5
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("cost = %f, want %f", got, want)
	}
	cfg.FastPricingMode = "source"
	got = calculateCost(p, detail, "priority", cfg)
	want = (80_000.0*10 + 200_000.0*1 + 20_000.0*12.5 + 10_000.0*60) / 1_000_000
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("source fast cost = %f, want %f", got, want)
	}
}

func TestDecodeCatalog(t *testing.T) {
	raw := `{"providers":{"openai":{"models":{"gpt-test":{"cost":{"input":1,"output":2,"cache_read":0.1,"cache_write":0.2,"tiers":[{"input":3,"output":4,"cache_read":0.3,"cache_write":0.4,"tier":{"type":"context","size":272000}}]},"experimental":{"modes":{"fast":{"cost":{"input":2.5,"output":5,"cache_read":0.25,"cache_write":0.5}}}}}}}}}`
	prices, err := decodeCatalog(strings.NewReader(raw))
	if err != nil || len(prices) != 1 {
		t.Fatalf("decode: %v %#v", err, prices)
	}
	p := prices[0]
	if p.LongInput != 3 || p.FastOutput != 5 || p.CacheWrite != .2 {
		t.Fatalf("price = %#v", p)
	}
}
