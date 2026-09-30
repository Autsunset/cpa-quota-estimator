package main

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestGPT6OfficialPricing(t *testing.T) {
	for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
		p, ok := officialGPT6Price("openai/" + model)
		if !ok {
			t.Fatal(model)
		}
		scale := 1.0
		if model == "gpt-6-luna" {
			scale = .05
		}
		for _, mode := range []string{pricingModeAPI, pricingModeCurrentAPI, pricingModeLegacyAPI} {
			cfg := defaultConfig()
			cfg.PricingMode = mode
			for _, tc := range []struct {
				tier  string
				input int64
				want  float64
			}{
				{"default", 272000, 1.459},
				{"default", 272001, 2.418004},
				{"fast", 272001, 4.836008},
				{"priority", 272001, 4.836008},
				{"flex", 272001, 1.209002},
				{"batch", 272001, 1.209002},
			} {
				d := usageDetail{InputTokens: tc.input, CacheReadTokens: 50000, CacheCreationTokens: 10000, OutputTokens: 100000}
				got := calculateCost(p, d, tc.tier, cfg)
				if math.Abs(got-tc.want*scale) > 1e-9 {
					t.Fatalf("%s/%s/%s: got %f want %f", model, mode, tc.tier, got, tc.want*scale)
				}
			}
			if got := calculateCost(p, usageDetail{InputTokens: 1000000}, "fast", cfg); math.Abs(got-8*scale) > 1e-9 {
				t.Fatal(got)
			}
		}
	}
}

func TestGPT6CatalogAndAllowances(t *testing.T) {
	prices, err := decodeCatalog(strings.NewReader(`{"providers":{"openai":{"models":{"gpt-6-sol":{"cost":{"input":99}},"gpt-6.1-sol":{"cost":{"input":99,"cache_read":99}},"gpt-6-luna":{"cost":{"input":99}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStore(filepath.Join(t.TempDir(), "prices.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	if err = seedPrices(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err = s.upsertPrices(ctx, prices); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeCurrentAPI
	rows, err := s.remainingModelAllowances(ctx, 10, cfg)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, row := range rows {
		switch row.Model {
		case "gpt-6-sol":
			found++
			if row.InputTokens != 5000000 || row.OutputTokens != 1000000 || row.CacheReadTokens != 50000000 {
				t.Fatal(row)
			}
		case "gpt-6.1-sol":
			found++
			if row.InputTokens != 5000000 || row.OutputTokens != 1000000 || row.CacheReadTokens != 100000000 {
				t.Fatal(row)
			}
		case "gpt-6-luna":
			found++
			if row.InputTokens != 100000000 || row.OutputTokens != 20000000 || row.CacheReadTokens != 1000000000 {
				t.Fatal(row)
			}
		}
	}
	if found != 3 {
		t.Fatal(rows)
	}
}

func TestGPT61SolPricing(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "prices.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	if err = seedPrices(ctx, s); err != nil {
		t.Fatal(err)
	}
	p, found, err := s.getPrice(ctx, " OpenAI/GPT-6.1-Sol ")
	if err != nil || !found {
		t.Fatalf("seeded price missing: found=%v err=%v", found, err)
	}
	for _, tc := range []struct {
		mode, tier string
		input      int64
		want       float64
	}{
		{pricingModeAPI, "default", 272000, 1.454},
		{pricingModeAPI, "default", 272001, 2.408004},
		{pricingModeAPI, "fast", 272001, 4.816008},
		{pricingModeAPI, "priority", 272001, 4.816008},
		{pricingModeAPI, "flex", 272001, 1.204002},
		{pricingModeAPI, "batch", 272001, 1.204002},
		{pricingModeCredits, "default", 272000, 35.725},
		{pricingModeCredits, "default", 272001, 35.72505},
	} {
		cfg := defaultConfig()
		cfg.PricingMode = tc.mode
		d := usageDetail{InputTokens: tc.input, CacheReadTokens: 50000, CacheCreationTokens: 10000, OutputTokens: 100000}
		if got := calculateCost(p, d, tc.tier, cfg); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("%s/%s/%d: got %g want %g", tc.mode, tc.tier, tc.input, got, tc.want)
		}
	}
	cfg := defaultConfig()
	rows, err := s.remainingModelAllowances(ctx, 250, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Model == "gpt-6.1-sol" {
			if row.InputTokens != 5000000 || row.OutputTokens != 1000000 || row.CacheReadTokens != 100000000 {
				t.Fatal(row)
			}
			return
		}
	}
	t.Fatal("GPT-6.1 Sol missing from credit allowances")
}
