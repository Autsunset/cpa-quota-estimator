package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestPricingSettingsPersistAndRecalculateHistory(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "pricing-settings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	if err = s.upsertPrices(ctx, []price{{
		Model: "gpt-test", Input: 1, Output: 2,
		LongInput: 2, LongOutput: 3,
		Source: "test", UpdatedAt: time.Now().Unix(),
	}}); err != nil {
		t.Fatal(err)
	}
	cycleResult, err := s.db.Exec(`INSERT INTO quota_cycles(account,started_at,reset_at,window_minutes) VALUES('account',100,1000,15)`)
	if err != nil {
		t.Fatal(err)
	}
	cycleID, err := cycleResult.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,requested_at,account,model,service_tier,input_tokens,output_tokens,total_tokens,cost_usd,quota_scope) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		cycleID, 200, "account", "gpt-test", "fast", 300_000, 10_000, 310_000, 99, mainQuotaScope); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,window_tokens,window_cost_usd,requests) VALUES(?,?,?,?,?,?,?,?,?)`,
		cycleID, 200, "account", 10, 1000, 15, 310_000, 99, 1); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig().withPricingSettings(pricingSettings{})
	count, err := s.savePricingSettingsAndRecalculate(ctx, cfg.pricingSettings(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("recalculated events = %d, want 1", count)
	}
	assertStoredCosts(t, s, 0.32)

	saved, err := s.loadPricingSettings(ctx, pricingSettings{ApplyLongContext: true, ApplyFast: true})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ApplyLongContext || saved.ApplyFast {
		t.Fatalf("saved settings = %#v, want both disabled", saved)
	}

	enabled := pricingSettings{ApplyLongContext: true, ApplyFast: true}
	cfg = defaultConfig().withPricingSettings(enabled)
	if _, err = s.savePricingSettingsAndRecalculate(ctx, enabled, cfg); err != nil {
		t.Fatal(err)
	}
	assertStoredCosts(t, s, 1.575)
}

func assertStoredCosts(t *testing.T, s *store, want float64) {
	t.Helper()
	var eventCost, sampleCost float64
	if err := s.db.QueryRow(`SELECT cost_usd FROM usage_events LIMIT 1`).Scan(&eventCost); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT window_cost_usd FROM quota_samples LIMIT 1`).Scan(&sampleCost); err != nil {
		t.Fatal(err)
	}
	if math.Abs(eventCost-want) > 1e-9 || math.Abs(sampleCost-want) > 1e-9 {
		t.Fatalf("event cost = %f, sample cost = %f, want %f", eventCost, sampleCost, want)
	}
}

func TestPricingModePersistsAndCanSwitchBack(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "pricing-mode-switch.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = s.upsertPrices(ctx, []price{{Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: .4, CacheWrite: 5}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO usage_events(requested_at,account,model,input_tokens,output_tokens,cache_read_tokens,total_tokens,quota_scope) VALUES(100,'account','gpt-5.6-sol',1000000,100000,500000,1100000,?)`, mainQuotaScope); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO quota_samples(sampled_at,account,used_percent,reset_at,window_minutes,window_cost_usd) VALUES(100,'account',1,1000,15,99)`); err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		mode string
		want float64
	}{{pricingModeLegacyAPI, 5.75}, {pricingModeCredits, 143.75}, {pricingModeCurrentAPI, 4.2}}
	for _, check := range checks {
		settings := pricingSettings{PricingMode: check.mode}
		cfg := defaultConfig().withPricingSettings(settings)
		if _, err = s.savePricingSettingsAndRecalculate(ctx, settings, cfg); err != nil {
			t.Fatal(err)
		}
		assertStoredCosts(t, s, check.want)
		saved, errLoad := s.loadPricingSettings(ctx, defaultConfig().pricingSettings())
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if saved.PricingMode != check.mode {
			t.Fatalf("saved mode = %q, want %q", saved.PricingMode, check.mode)
		}
	}
}

func TestPricingModeRecalculatesEveryHistoricalCycle(t *testing.T) {
	ctx := context.Background()
	s, err := openStore(filepath.Join(t.TempDir(), "pricing-mode-history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = s.upsertPrices(ctx, []price{{Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: .4}}); err != nil {
		t.Fatal(err)
	}
	var cycleIDs []int64
	for index := 0; index < 2; index++ {
		start := int64(100 + index*1000)
		result, errInsert := s.db.Exec(`INSERT INTO quota_cycles(account,started_at,reset_at,window_minutes,ended_at) VALUES(?,?,?,?,?)`, "account", start, start+900, 15, start+900)
		if errInsert != nil {
			t.Fatal(errInsert)
		}
		cycleID, errID := result.LastInsertId()
		if errID != nil {
			t.Fatal(errID)
		}
		cycleIDs = append(cycleIDs, cycleID)
		if _, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,requested_at,account,model,input_tokens,output_tokens,cache_read_tokens,total_tokens,quota_scope) VALUES(?,?,?,?,?,?,?,?,?)`,
			cycleID, start+100, "account", "gpt-5.6-sol", 1_000_000, 100_000, 500_000, 1_100_000, mainQuotaScope); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(`INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,window_tokens,window_cost_usd,requests) VALUES(?,?,?,?,?,?,?,?,?)`,
			cycleID, start+100, "account", 10, start+900, 15, 1_100_000, 99, 1); err != nil {
			t.Fatal(err)
		}
	}

	for _, check := range []struct {
		mode string
		want float64
	}{{pricingModeCredits, 143.75}, {pricingModeLegacyAPI, 5.75}, {pricingModeCurrentAPI, 4.2}} {
		settings := pricingSettings{PricingMode: check.mode}
		cfg := defaultConfig().withPricingSettings(settings)
		if _, err = s.savePricingSettingsAndRecalculate(ctx, settings, cfg); err != nil {
			t.Fatal(err)
		}
		for _, cycleID := range cycleIDs {
			points, _, errPoints := s.pointsForCycle(ctx, "account", cycleID, 10)
			if errPoints != nil {
				t.Fatal(errPoints)
			}
			if len(points) != 1 || math.Abs(points[0].WindowCostUSD-check.want) > 1e-9 {
				t.Fatalf("mode=%s cycle=%d points=%#v want value=%f", check.mode, cycleID, points, check.want)
			}
		}
	}
}
