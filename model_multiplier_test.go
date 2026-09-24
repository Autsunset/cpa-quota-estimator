package main

import (
	"math"
	"testing"
)

func testAnchorFit() *weightFit {
	return &weightFit{Available: true, Models: []learnedModelWeights{
		{Model: "gpt-5.6-sol", Input: weightEstimate{Value: 1, Low: 1, High: 1, Identified: true}},
		{Model: "gpt-6-astra", Input: weightEstimate{Value: 3, Low: 2.4, High: 3.6, Identified: true}},
		{Model: "gpt-6-sol", Input: weightEstimate{Value: .5, Low: .5, High: .5, PriorLocked: true}},
	}}
}

func TestAnchoredPricesUseOfficialShapeAndRelativeAdjustment(t *testing.T) {
	ref := price{Model: "gpt-5.6-sol", Input: 4, CacheRead: .4, Output: 20, CacheWrite: 5}
	astra := price{Model: "gpt-6-astra", Input: 10, CacheRead: 1, Output: 50, CacheWrite: 12.5}
	sol := price{Model: "gpt-6-sol", Input: 2, CacheRead: .2, Output: 10, CacheWrite: 2.5}
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeAPI
	cfg.AnchorModel = "gpt-5.6-sol"
	cfg.LearnedFit = testAnchorFit()
	cfg.PriceCatalog = map[string]price{"gpt-5.6-sol": ref, "gpt-6-astra": astra, "gpt-6-sol": sol}
	got, adjustment := cfg.effectiveModelPrice(astra)
	for _, check := range []struct{ value, want float64 }{{got.Input, 12}, {got.CacheRead, 1.2}, {got.Output, 60}, {got.CacheWrite, 15}} {
		if math.Abs(check.value-check.want) > 1e-9 {
			t.Fatalf("adjusted rate=%f want=%f", check.value, check.want)
		}
	}
	if !adjustment.Calibrated || adjustment.Baseline || math.Abs(adjustment.Factor-1.2) > 1e-9 {
		t.Fatalf("adjustment=%#v", adjustment)
	}
	cfg.AnchorModel = "gpt-6-astra"
	anchor, anchorInfo := cfg.effectiveModelPrice(astra)
	if anchor.Input != astra.Input || anchor.CacheRead != astra.CacheRead || anchor.Output != astra.Output || !anchorInfo.Baseline || anchorInfo.Factor != 1 {
		t.Fatalf("anchor=%#v info=%#v", anchor, anchorInfo)
	}
	other, _ := cfg.effectiveModelPrice(ref)
	if math.Abs(other.Input-4/1.2) > 1e-9 {
		t.Fatalf("relative anchor price=%f", other.Input)
	}
	locked, lockInfo := cfg.effectiveModelPrice(sol)
	if math.Abs(locked.Input-2/1.2) > 1e-9 || lockInfo.Calibrated {
		t.Fatalf("locked target=%#v info=%#v", locked, lockInfo)
	}
	cfg.PricingMode = pricingModeCredits
	cfg.AnchorModel = "gpt-5.6-sol"
	credits, _ := cfg.effectiveModelPrice(astra)
	if math.Abs(credits.Input-300) > 1e-9 || math.Abs(credits.Output-1500) > 1e-9 {
		t.Fatalf("credits=%#v", credits)
	}
	cfg.AnchorModel = "gpt-6-astra"
	anchorCredits, _ := cfg.effectiveModelPrice(astra)
	if anchorCredits.Input != 250 || anchorCredits.Output != 1250 {
		t.Fatalf("credits anchor=%#v", anchorCredits)
	}
}

func TestCustomPricesIgnoreLearnerAndAnchor(t *testing.T) {
	astra := price{Model: "gpt-6-astra", Input: 10, CacheRead: 1, Output: 50, CacheWrite: 12.5, LongInput: 20, LongRead: 2, LongOutput: 75, LongWrite: 25}
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeCustom
	cfg.LearnedFit = testAnchorFit()
	cfg.CustomFastMultiplier = 3
	cfg.CustomLongContext = false
	cfg.CustomPrices = map[string]customModelPrice{"gpt-6-astra": {Input: 7, CacheRead: .7, Output: 35, CacheWrite: 8}}
	for _, anchor := range []string{"gpt-5.6-sol", "gpt-6-astra"} {
		cfg.AnchorModel = anchor
		p, adjustment := cfg.effectiveModelPrice(astra)
		if p.Input != 7 || p.CacheRead != .7 || p.Output != 35 || p.CacheWrite != 8 || adjustment.Factor != 1 {
			t.Fatalf("custom=%#v adjustment=%#v", p, adjustment)
		}
		cost := calculateCost(astra, usageDetail{InputTokens: 1_000_000}, "fast", cfg)
		if math.Abs(cost-21) > 1e-9 {
			t.Fatalf("custom fast cost=%f", cost)
		}
	}
}

func TestFastAndLongShowLearnedAgainstOfficial(t *testing.T) {
	p, _ := officialGPT6Price("gpt-6-astra")
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeAPI
	cfg.LearnedFit = testAnchorFit()
	cfg.LearnedFit.Fast = weightEstimate{Value: 3, Low: 2.5, High: 3.5}
	cfg.LearnedFit.LongContext = weightEstimate{Value: 1.2, Low: 1, High: 1.4}
	row := cfg.priceRow(p)
	if row.OfficialFastMultiplier != 2 || row.FastMultiplier != 3 || row.OfficialLongMultiplier != 2 || math.Abs(row.LongMultiplier-1.2) > 1e-9 || math.Abs(row.LongUncertaintyPercent-100.0/6) > 1e-9 {
		t.Fatalf("multipliers=%#v", row)
	}
	got := calculateCost(p, usageDetail{InputTokens: 1_000_000}, "fast", cfg)
	if math.Abs(got-43.2) > 1e-9 {
		t.Fatalf("learned fast/long cost=%f", got)
	}
}

func TestLongContextUsesLearnedOrOfficialTier(t *testing.T) {
	p, _ := officialGPT6Price("gpt-6-astra")
	detail := usageDetail{InputTokens: 1_000_000, CacheReadTokens: 200_000, CacheCreationTokens: 100_000, OutputTokens: 50_000}
	for _, tc := range []struct {
		name, mode string
		locked     bool
		want       float64
		multiplier float64
	}{
		{"api_learned", pricingModeAPI, false, (700_000*12 + 200_000*1.2 + 100_000*15 + 50_000*60) / 1_000_000 * 1.2, 1.2},
		{"api_locked", pricingModeAPI, true, (700_000*24 + 200_000*2.4 + 100_000*30 + 50_000*90) / 1_000_000, 2},
		{"credits_learned", pricingModeCredits, false, (700_000*300 + 200_000*30 + 100_000*0 + 50_000*1500) / 1_000_000 * 1.2, 1.2},
		{"credits_locked", pricingModeCredits, true, (700_000*300 + 200_000*30 + 100_000*0 + 50_000*1500) / 1_000_000, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.PricingMode = tc.mode
			cfg.PriceCatalog = map[string]price{"gpt-5.6-sol": {Model: "gpt-5.6-sol", Input: 4}, p.Model: p}
			cfg.LearnedFit = testAnchorFit()
			cfg.LearnedFit.LongContext = weightEstimate{Value: 1.2, Low: 1, High: 1.4, PriorLocked: tc.locked}
			got := calculateCost(p, detail, "", cfg)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("cost=%f want=%f", got, tc.want)
			}
			row := cfg.priceRow(p)
			if math.Abs(row.LongMultiplier-tc.multiplier) > 1e-9 || (row.LongUncertaintyPercent > 0) == tc.locked {
				t.Fatalf("long price row=%#v", row)
			}
		})
	}
}

func TestFastUsesLearnedOrOfficialMultiplierWithoutStacking(t *testing.T) {
	p, _ := officialGPT6Price("gpt-6-astra")
	for _, mode := range []string{pricingModeAPI, pricingModeCredits} {
		for _, locked := range []bool{false, true} {
			cfg := defaultConfig()
			cfg.PricingMode = mode
			cfg.LongContextThreshold = 2_000_000
			cfg.PriceCatalog = map[string]price{"gpt-5.6-sol": {Model: "gpt-5.6-sol", Input: 4}, p.Model: p}
			cfg.LearnedFit = testAnchorFit()
			cfg.LearnedFit.Fast = weightEstimate{Value: 3, PriorLocked: locked}
			standard := calculateCost(p, usageDetail{InputTokens: 1_000_000}, "", cfg)
			got := calculateCost(p, usageDetail{InputTokens: 1_000_000}, "fast", cfg)
			want := 3.0
			if locked {
				want = cfg.officialFastMultiplier(p)
			}
			if math.Abs(got-standard*want) > 1e-9 {
				t.Fatalf("mode=%s locked=%v fast=%f standard=%f want ratio=%f", mode, locked, got, standard, want)
			}
		}
	}
}

func TestUnlistedCreditsPriceIsMarkedAsEstimate(t *testing.T) {
	cfg := defaultConfig()
	row := cfg.priceRow(price{Model: "gpt-unlisted", Input: 3, CacheRead: .3, Output: 15, CacheWrite: 4})
	if row.OfficialListed || row.Official.Input != 75 || row.Official.CacheWrite != 0 {
		t.Fatalf("fallback row=%#v", row)
	}
}

func TestAdjustmentUsesCurrentReferencePrice(t *testing.T) {
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeAPI
	cfg.LearnedFit = testAnchorFit()
	astra := price{Model: "gpt-6-astra", Input: 10, CacheRead: 1, Output: 50}
	cfg.PriceCatalog = map[string]price{"gpt-5.6-sol": {Model: "gpt-5.6-sol", Input: 5}, "gpt-6-astra": astra}
	p, _ := cfg.effectiveModelPrice(astra)
	if math.Abs(p.Input-15) > 1e-9 {
		t.Fatalf("input=%f want 15 with current reference price 5", p.Input)
	}
	delete(cfg.PriceCatalog, "gpt-5.6-sol")
	p, _ = cfg.effectiveModelPrice(astra)
	if math.Abs(p.Input-12) > 1e-9 {
		t.Fatalf("fallback input=%f want 12", p.Input)
	}
}
