package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
)

func TestRemainingModelAllowancesFollowSelectedPricingMode(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "allowances.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	if err = s.upsertPrices(ctx, []price{
		{Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: .4},
		{Model: "gpt-5.6-terra", Input: 2, Output: 12, CacheRead: .2},
		{Model: "gpt-5.6-luna", Input: .2, Output: 1.2, CacheRead: .02},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeCredits
	rows, err := s.remainingModelAllowances(ctx, 125, cfg)
	if err != nil {
		t.Fatal(err)
	}
	byModel := make(map[string]modelAllowance)
	for _, row := range rows {
		byModel[row.Model] = row
	}
	checks := []struct {
		model                 string
		input, output, cached float64
	}{
		{"gpt-5.6-sol", 1_000_000, 125.0e6 / 750, 10_000_000},
		{"gpt-5.6-terra", 2_000_000, 125.0e6 / 375, 20_000_000},
		{"gpt-5.6-luna", 5_000_000, 125.0e6 / 150, 50_000_000},
	}
	for _, check := range checks {
		row, ok := byModel[check.model]
		if !ok {
			t.Fatalf("missing allowance for %s", check.model)
		}
		if row.ValueUnit != "credits" || math.Abs(row.InputTokens-check.input) > 1e-6 || math.Abs(row.OutputTokens-check.output) > 1e-6 || math.Abs(row.CacheReadTokens-check.cached) > 1e-6 {
			t.Fatalf("%s allowance = %#v", check.model, row)
		}
	}
}
