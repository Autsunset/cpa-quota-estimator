package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestPricingModeMigrationAndSavedAnchor(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "pricing-migration.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	for old, want := range map[string]string{"legacy_api": "api", "current_api": "api", "credits": "credits", "learned": "credits", "api": "api", "custom": "custom"} {
		raw, _ := json.Marshal(map[string]any{"pricing_mode": old, "anchor_model": "gpt-6-astra"})
		if _, err = s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, pricingSettingsMetadataKey, string(raw)); err != nil {
			t.Fatal(err)
		}
		got, err := s.loadPricingSettings(ctx, defaultConfig().pricingSettings())
		if err != nil {
			t.Fatal(err)
		}
		if got.PricingMode != want || got.AnchorModel != "gpt-6-astra" || got.Migrated != (old != want) {
			t.Fatalf("%s -> %#v", old, got)
		}
		if normalizePricingMode(old) != want || !validPricingMode(old) {
			t.Fatalf("compatibility mapping for %s", old)
		}
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE metadata SET value=? WHERE key=?`, `{"pricing_mode":"legacy_api"}`, pricingSettingsMetadataKey); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadPricingSettings(ctx, defaultConfig().pricingSettings())
	if err != nil || got.AnchorModel != "gpt-5.6-sol" {
		t.Fatalf("default anchor=%#v err=%v", got, err)
	}
}

func TestDashboardUsesThreePricingChoices(t *testing.T) {
	html := string(dashboardHTML)
	for _, mode := range []string{`name="pricingMode" value="api"`, `name="pricingMode" value="credits"`, `name="pricingMode" value="custom"`, `id="anchorModel"`, `id="customPriceRows"`, `id="pricingTaskProgress"`} {
		if !strings.Contains(html, mode) {
			t.Fatalf("missing %s", mode)
		}
	}
	for _, obsolete := range []string{`id="astraMultiplier"`, `id="applyModelCalibration"`, `id="applyFastPricing"`, `id="applyLongContextPricing"`, `value="legacy_api"`, `value="current_api"`, `value="learned"`} {
		if strings.Contains(html, obsolete) {
			t.Fatalf("obsolete control %s", obsolete)
		}
	}
}

func TestExistingCreditsHistoryTriggersOneTimeFormulaMigration(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "formula.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	if _, err = s.db.Exec(`INSERT INTO usage_events(account,requested_at,model,input_tokens,cost_usd) VALUES('a',100,'gpt-5.6-sol',1000000,100)`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO metadata(key,value) VALUES(?,?)`, pricingSettingsMetadataKey, `{"pricing_mode":"credits"}`); err != nil {
		t.Fatal(err)
	}
	settings, err := s.loadPricingSettings(ctx, defaultConfig().pricingSettings())
	if err != nil || !settings.Migrated {
		t.Fatalf("upgrade=%#v err=%v", settings, err)
	}
	if err = seedPrices(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err = s.savePricingSettingsAndRecalculate(ctx, settings, defaultConfig()); err != nil {
		t.Fatal(err)
	}
	again, err := s.loadPricingSettings(ctx, defaultConfig().pricingSettings())
	if err != nil || again.Migrated {
		t.Fatalf("repeated migration=%#v err=%v", again, err)
	}
}
