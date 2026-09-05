package main

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func astraTestPrice() price {
	return price{Model: "gpt-6-astra", Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5, LongInput: 20, LongOutput: 75, LongRead: 2, LongWrite: 25, FastInput: 20, FastOutput: 100, FastRead: 2, FastWrite: 25}
}

func TestAstraMultiplierPreservesBasePricesAndAppliesExactlyOnce(t *testing.T) {
	p := astraTestPrice()
	for _, mode := range []string{pricingModeCurrentAPI, pricingModeLegacyAPI, pricingModeCredits} {
		for _, fastMode := range []string{"source", "multiplier"} {
			for _, long := range []bool{false, true} {
				for _, fast := range []bool{false, true} {
					cfg := defaultConfig()
					cfg.PricingMode = mode
					cfg.FastPricingMode = fastMode
					cfg.ApplyLongContextPricing = long
					cfg.ApplyFastPricing = fast
					for _, tier := range []string{"auto", "fast", "priority"} {
						d := usageDetail{InputTokens: 300000, OutputTokens: 10000, CacheReadTokens: 200000, CacheCreationTokens: 10000}
						// This otherwise identical model has no quota calibration.
						base := p
						base.Model = "gpt-test"
						got := calculateCost(p, d, tier, cfg)
						want := calculateCost(base, d, tier, cfg) * 1.8
						if math.Abs(got-want) > 1e-9 {
							t.Fatalf("%s/%s/%t/%t/%s got %f want %f", mode, fastMode, long, fast, tier, got, want)
						}
					}
				}
			}
		}
	}
	if p != astraTestPrice() {
		t.Fatal("base prices mutated")
	}
	if got := priceForPricingMode(p, pricingModeCurrentAPI); got != p {
		t.Fatal("current API base prices must remain uncalibrated")
	}
	if defaultConfig().modelPriceMultiplier(" openai/GPT-6-ASTRA ") != 1.8 || defaultConfig().modelPriceMultiplier("gpt-5.6-sol") != 1 {
		t.Fatal("model normalization or scope")
	}
}

func TestAstraMigrationAndAllowances(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "multiplier.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = s.upsertPrices(ctx, []price{astraTestPrice(), {Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: .4}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes) VALUES(1,'a',100,1000,15),(2,'a',1000,1900,15);
 INSERT INTO usage_events(cycle_id,requested_at,account,model,input_tokens,output_tokens,cache_read_tokens,cost_usd) VALUES
 (1,200,'a','gpt-6-astra',1000000,100000,500000,10.5),
 (1,210,'a','gpt-5.6-sol',1000000,100000,500000,5.75),
 (2,1100,'a','openai/gpt-6-astra',1000000,100000,500000,10.5);
 INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,window_cost_usd) VALUES
 (1,220,'a',1,1000,15,16.25),(2,1200,'a',1,1900,15,10.5);`); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeLegacyAPI
	for i := 0; i < 2; i++ {
		if err = s.ensureAstraCalibration(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		var astra, sol float64
		if err = s.db.QueryRow(`SELECT cost_usd FROM usage_events WHERE model='gpt-6-astra'`).Scan(&astra); err != nil {
			t.Fatal(err)
		}
		if err = s.db.QueryRow(`SELECT cost_usd FROM usage_events WHERE model='gpt-5.6-sol'`).Scan(&sol); err != nil {
			t.Fatal(err)
		}
		if math.Abs(astra-18.9) > 1e-9 || sol != 5.75 {
			t.Fatalf("migration %d: astra=%f sol=%f", i, astra, sol)
		}
		for cid, want := range map[int]float64{1: 24.65, 2: 18.9} {
			var v float64
			if err = s.db.QueryRow(`SELECT window_cost_usd FROM quota_samples WHERE cycle_id=?`, cid).Scan(&v); err != nil {
				t.Fatal(err)
			}
			if math.Abs(v-want) > 1e-9 {
				t.Fatalf("sample cycle=%d got=%f want=%f", cid, v, want)
			}
		}
	}
	rows, err := s.remainingModelAllowances(ctx, 18, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Model != "gpt-6-astra" {
		t.Fatalf("rows=%#v", rows)
	}
	a := rows[0]
	if a.InputRate != 10 || a.OutputRate != 50 || a.CacheReadRate != 1 || a.ModelMultiplier != 1.8 || a.InputTokens != 1000000 || a.CacheReadTokens != 10000000 || a.OutputTokens != 200000 {
		t.Fatalf("allowance=%#v", a)
	}
	// A price sync and repeated full recalculations never multiply stored prices.
	if err = s.upsertPrices(ctx, []price{astraTestPrice()}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = s.savePricingSettingsAndRecalculate(ctx, cfg.pricingSettings(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	var v float64
	if err = s.db.QueryRow(`SELECT cost_usd FROM usage_events WHERE model='gpt-6-astra'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if math.Abs(v-18.9) > 1e-9 {
		t.Fatalf("recalculated=%f", v)
	}
	for _, text := range []string{"model_multiplier", "model_price_multipliers", "Model multiplier ×{multiplier}", "模型倍率 ×{multiplier}", "allowanceRate"} {
		if !strings.Contains(string(dashboardHTML), text) {
			t.Fatalf("missing UI %s", text)
		}
	}
}

func TestAstraMigrationRollsBackAtomically(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "rollback.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = s.upsertPrices(ctx, []price{astraTestPrice()}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,requested_at,account,model,input_tokens,cost_usd) VALUES(1,100,'a','gpt-6-astra',1000000,10);
 INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,window_cost_usd) VALUES(1,100,'a',1,1000,15,10);
 CREATE TRIGGER reject_sample_update BEFORE UPDATE ON quota_samples BEGIN SELECT RAISE(ABORT,'test rollback'); END;`); err != nil {
		t.Fatal(err)
	}
	if err = s.ensureAstraCalibration(ctx, defaultConfig()); err == nil {
		t.Fatal("expected migration failure")
	}
	var v float64
	if err = s.db.QueryRow(`SELECT cost_usd FROM usage_events`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 10 {
		t.Fatalf("partial migration leaked: %f", v)
	}
	var n int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM metadata WHERE key=?`, astraCalibrationKey).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed migration marked applied")
	}
}
