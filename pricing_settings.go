package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

const pricingSettingsMetadataKey = "pricing_settings"
const pricingFormulaMetadataKey = "pricing_formula_v015"

type pricingSettings struct {
	PricingMode          string                      `json:"pricing_mode"`
	AnchorModel          string                      `json:"anchor_model"`
	CustomPrices         map[string]customModelPrice `json:"custom_prices,omitempty"`
	CustomFastMultiplier float64                     `json:"custom_fast_multiplier"`
	CustomLongContext    bool                        `json:"custom_long_context"`
	CustomLongThreshold  int64                       `json:"custom_long_threshold"`
	Migrated             bool                        `json:"-"`
	AutoFitAt            int64                       `json:"-"`
}

type pricingSettingsUpdate struct {
	PricingMode          string                      `json:"pricing_mode"`
	AnchorModel          string                      `json:"anchor_model"`
	CustomPrices         map[string]customModelPrice `json:"custom_prices"`
	CustomFastMultiplier *float64                    `json:"custom_fast_multiplier"`
	CustomLongContext    *bool                       `json:"custom_long_context"`
	CustomLongThreshold  *int64                      `json:"custom_long_threshold"`
	RestoreOfficial      bool                        `json:"restore_official"`
}

type pricingRecalcProgress struct {
	Stage          string  `json:"stage"`
	EventsDone     int64   `json:"events_done"`
	EventsTotal    int64   `json:"events_total"`
	SamplesDone    int64   `json:"samples_done"`
	SamplesTotal   int64   `json:"samples_total"`
	MaxBatchLockMS float64 `json:"max_batch_lock_ms"`
	BatchCount     int64   `json:"batch_count"`
}

func (c config) pricingSettings() pricingSettings {
	return pricingSettings{PricingMode: normalizePricingMode(c.PricingMode), AnchorModel: normalizeModel(c.AnchorModel),
		CustomPrices: c.CustomPrices, CustomFastMultiplier: c.CustomFastMultiplier, CustomLongContext: c.CustomLongContext, CustomLongThreshold: c.CustomLongThreshold, Migrated: c.PricingMigration}
}

func (c config) withPricingSettings(settings pricingSettings) config {
	c.PricingMode = normalizePricingMode(settings.PricingMode)
	c.AnchorModel = normalizeModel(settings.AnchorModel)
	if c.AnchorModel == "" {
		c.AnchorModel = "gpt-5.6-sol"
	}
	c.CustomPrices = settings.CustomPrices
	c.CustomFastMultiplier = settings.CustomFastMultiplier
	if c.CustomFastMultiplier <= 0 {
		c.CustomFastMultiplier = 2
	}
	c.CustomLongContext = settings.CustomLongContext
	c.CustomLongThreshold = settings.CustomLongThreshold
	if c.CustomLongThreshold <= 0 {
		c.CustomLongThreshold = 272000
	}
	return c
}

func normalizePricingSettings(settings pricingSettings) (pricingSettings, error) {
	settings.PricingMode = normalizePricingMode(settings.PricingMode)
	settings.AnchorModel = normalizeModel(settings.AnchorModel)
	if settings.AnchorModel == "" {
		settings.AnchorModel = "gpt-5.6-sol"
	}
	if settings.CustomFastMultiplier <= 0 {
		settings.CustomFastMultiplier = 2
	}
	if settings.CustomLongThreshold <= 0 {
		settings.CustomLongThreshold = 272000
	}
	if math.IsNaN(settings.CustomFastMultiplier) || math.IsInf(settings.CustomFastMultiplier, 0) || settings.CustomFastMultiplier < .1 || settings.CustomFastMultiplier > 100 {
		return settings, fmt.Errorf("custom_fast_multiplier must be between 0.1 and 100")
	}
	if settings.CustomLongThreshold < 1024 || settings.CustomLongThreshold > 10_000_000 {
		return settings, fmt.Errorf("custom_long_threshold must be between 1024 and 10000000")
	}
	normalized := make(map[string]customModelPrice, len(settings.CustomPrices))
	for model, p := range settings.CustomPrices {
		name := normalizeModel(model)
		if name == "" {
			return settings, fmt.Errorf("custom price model is required")
		}
		if err := validateCustomModelPrice(p); err != nil {
			return settings, fmt.Errorf("%s: %w", name, err)
		}
		normalized[name] = p
	}
	settings.CustomPrices = normalized
	return settings, nil
}

func (s *store) loadPricingSettings(ctx context.Context, fallback pricingSettings) (pricingSettings, error) {
	fallback, _ = normalizePricingSettings(fallback)
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, pricingSettingsMetadataKey).Scan(&raw)
	if err == sql.ErrNoRows {
		var eventCount int64
		if countErr := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events`).Scan(&eventCount); countErr != nil {
			return fallback, countErr
		}
		if eventCount > 0 {
			fallback.Migrated = true
		}
		return fallback, nil
	}
	if err != nil {
		return fallback, err
	}
	settings := fallback
	if err = json.Unmarshal([]byte(raw), &settings); err != nil {
		return fallback, fmt.Errorf("decode saved pricing settings: %w", err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal([]byte(raw), &fields); err != nil {
		return fallback, err
	}
	var oldMode string
	if encoded, ok := fields["pricing_mode"]; ok {
		_ = json.Unmarshal(encoded, &oldMode)
	}
	if strings.TrimSpace(oldMode) == "" {
		oldMode = pricingModeLegacyAPI
	}
	settings.Migrated = normalizePricingMode(oldMode) != strings.ToLower(strings.TrimSpace(oldMode))
	settings.PricingMode = normalizePricingMode(oldMode)
	var formula string
	formulaErr := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, pricingFormulaMetadataKey).Scan(&formula)
	if formulaErr != nil && formulaErr != sql.ErrNoRows {
		return fallback, formulaErr
	}
	if formulaErr == sql.ErrNoRows {
		var eventCount int64
		if countErr := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events`).Scan(&eventCount); countErr != nil {
			return fallback, countErr
		}
		if eventCount > 0 {
			settings.Migrated = true
		}
	}
	return normalizePricingSettings(settings)
}

// recalculatePricing keeps the direct offline/test entry point. Management and
// migration tasks switch the active config before calling the batch engine.
func (s *store) recalculatePricing(ctx context.Context, settings pricingSettings, cfg config, progress func(pricingRecalcProgress) error) (int64, error) {
	return s.recalculatePricingBatches(ctx, settings, cfg, cfg, progress, nil)
}

// Retained for in-process migrations and offline analysis. Management saves
// use the background task path.
func (s *store) savePricingSettingsAndRecalculate(ctx context.Context, settings pricingSettings, cfg config) (int64, error) {
	return s.recalculatePricing(ctx, settings, cfg, nil)
}
