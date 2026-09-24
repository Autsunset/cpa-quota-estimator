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
	prices, err := decodeCatalog(strings.NewReader(`{"providers":{"openai":{"models":{"gpt-6-sol":{"cost":{"input":99}},"gpt-6-luna":{"cost":{"input":99}}}}}}`))
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
		case "gpt-6-luna":
			found++
			if row.InputTokens != 100000000 || row.OutputTokens != 20000000 || row.CacheReadTokens != 1000000000 {
				t.Fatal(row)
			}
		}
	}
	if found != 2 {
		t.Fatal(rows)
	}
}
