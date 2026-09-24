package main

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFitRepriceRequiresTwoPercentChangeAndSixHours(t *testing.T) {
	old := testAnchorFit()
	old.Fast = weightEstimate{Value: 2.5}
	old.LongContext = weightEstimate{Value: 1}
	current := *old
	current.Models = append([]learnedModelWeights(nil), old.Models...)
	cfg := defaultConfig()
	cfg.PricingMode = pricingModeAPI
	cfg.PriceCatalog = map[string]price{
		"gpt-5.6-sol": {Model: "gpt-5.6-sol", Input: 4},
		"gpt-6-astra": {Model: "gpt-6-astra", Input: 10},
	}
	cfg.LearnedFit = &current
	const now = int64(1_000_000)
	current.Models[1].Input.Value = 3.03 // 1% relative to the previous fit.
	if fitRepricingDue(old, cfg, 0, now) {
		t.Fatal("1% factor drift triggered repricing")
	}
	current.Models[1].Input.Value = 3.09
	if !fitRepricingDue(old, cfg, 0, now) {
		t.Fatal("3% factor drift was ignored")
	}
	if fitRepricingDue(old, cfg, now-fitRepriceMinInterval+1, now) {
		t.Fatal("six-hour rate limit was ignored")
	}
	if !fitRepricingDue(old, cfg, now-fitRepriceMinInterval, now) {
		t.Fatal("six-hour boundary did not permit repricing")
	}
	cfg.PricingMode = pricingModeCustom
	if fitRepricingDue(old, cfg, 0, now) {
		t.Fatal("custom basis used learner repricing")
	}
	cfg.PricingMode = pricingModeAPI
	current.Models[1].Input.Value = 3
	current.Fast.Value = 2.56
	if !fitRepricingDue(old, cfg, 0, now) {
		t.Fatal("Fast factor drift was ignored")
	}
	current.Fast.Value = 2.5
	current.LongContext.Value = 1.03
	if !fitRepricingDue(old, cfg, 0, now) {
		t.Fatal("long-context factor drift was ignored")
	}
}

func TestFitRepriceLimitSurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limit.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.saveFitRepriceAt(context.Background(), 123456); err != nil {
		t.Fatal(err)
	}
	s.close()
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	at, err := s.lastFitRepriceAt(context.Background())
	if err != nil || at != 123456 {
		t.Fatalf("persisted time=%d err=%v", at, err)
	}
}

func TestAutoFitRepricingPersistsLimitWithSettings(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "auto-fit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	settings := defaultConfig().pricingSettings()
	settings.AutoFitAt = 123456
	if _, err = s.recalculatePricing(context.Background(), settings, defaultConfig(), nil); err != nil {
		t.Fatal(err)
	}
	at, err := s.lastFitRepriceAt(context.Background())
	if err != nil || at != 123456 {
		t.Fatalf("auto repricing marker=%d err=%v", at, err)
	}
}
