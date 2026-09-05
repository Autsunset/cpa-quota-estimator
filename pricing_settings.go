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

type pricingSettings struct {
	ApplyModelCalibration bool    `json:"apply_model_calibration"`
	AstraMultiplier       float64 `json:"astra_multiplier"`
	ApplyLongContext      bool    `json:"apply_long_context_pricing"`
	ApplyFast             bool    `json:"apply_fast_pricing"`
	PricingMode           string  `json:"pricing_mode"`
}

type pricingSettingsUpdate struct {
	ApplyModelCalibration *bool    `json:"apply_model_calibration"`
	AstraMultiplier       *float64 `json:"astra_multiplier"`
	ApplyLongContext      *bool    `json:"apply_long_context_pricing"`
	ApplyFast             *bool    `json:"apply_fast_pricing"`
	PricingMode           string   `json:"pricing_mode"`
}

func (c config) pricingSettings() pricingSettings {
	return pricingSettings{
		ApplyModelCalibration: c.ApplyModelCalibration,
		AstraMultiplier:       c.AstraMultiplier,
		ApplyLongContext:      c.ApplyLongContextPricing,
		ApplyFast:             c.ApplyFastPricing,
		PricingMode:           normalizePricingMode(c.PricingMode),
	}
}

func (c config) withPricingSettings(settings pricingSettings) config {
	c.ApplyModelCalibration = settings.ApplyModelCalibration
	if settings.AstraMultiplier != 0 {
		c.AstraMultiplier = settings.AstraMultiplier
	}
	c.ApplyLongContextPricing = settings.ApplyLongContext
	c.ApplyFastPricing = settings.ApplyFast
	c.PricingMode = normalizePricingMode(settings.PricingMode)
	return c
}

func (s *store) loadPricingSettings(ctx context.Context, fallback pricingSettings) (pricingSettings, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, pricingSettingsMetadataKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return fallback, nil
	}
	if err != nil {
		return fallback, err
	}
	settings := pricingSettings{ApplyModelCalibration: fallback.ApplyModelCalibration, AstraMultiplier: fallback.AstraMultiplier}
	if settings.AstraMultiplier == 0 {
		settings.AstraMultiplier = defaultConfig().AstraMultiplier
	}
	if err = json.Unmarshal([]byte(raw), &settings); err != nil {
		return fallback, fmt.Errorf("decode saved pricing settings: %w", err)
	}
	if strings.TrimSpace(settings.PricingMode) == "" {
		settings.PricingMode = normalizePricingMode(fallback.PricingMode)
	} else {
		settings.PricingMode = normalizePricingMode(settings.PricingMode)
	}
	if err := validateAstraMultiplier(settings.AstraMultiplier); err != nil {
		return fallback, err
	}
	return settings, nil
}

func validateAstraMultiplier(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0.01 || value > 100 {
		return fmt.Errorf("astra_multiplier must be between 0.01 and 100")
	}
	return nil
}

type costRecalculationEvent struct {
	ID               int64
	Model            string
	ServiceTier      string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

func (s *store) savePricingSettingsAndRecalculate(ctx context.Context, settings pricingSettings, cfg config) (int64, error) {
	return s.recalculatePricing(ctx, settings, cfg, false)
}

const astraCalibrationKey = "astra_quota_multiplier_v1_1.8"

// Run once on upgrade; only Astra history changes. The marker, request values,
// and affected cycle samples commit together and are safe to retry.
func (s *store) ensureAstraCalibration(ctx context.Context, cfg config) error {
	var applied string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", astraCalibrationKey).Scan(&applied)
	if err == nil && applied == "applied" {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	_, err = s.recalculatePricing(ctx, cfg.pricingSettings(), cfg, true)
	return err
}

func (s *store) recalculatePricing(ctx context.Context, settings pricingSettings, cfg config, astraOnly bool) (int64, error) {
	// Internal callers written before calibration settings have a zero value.
	// HTTP/config input is validated separately and cannot save a zero rate.
	if settings.AstraMultiplier == 0 {
		settings.AstraMultiplier = cfg.AstraMultiplier
	}
	if settings.AstraMultiplier == 0 {
		settings.AstraMultiplier = defaultConfig().AstraMultiplier
	}
	if err := validateAstraMultiplier(settings.AstraMultiplier); err != nil {
		return 0, err
	}
	cfg = cfg.withPricingSettings(settings)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	prices := make(map[string]price)
	priceRows, err := tx.QueryContext(ctx, `SELECT model,input,output,cache_read,cache_write,long_input,long_output,long_cache_read,long_cache_write,fast_input,fast_output,fast_cache_read,fast_cache_write,source,updated_at FROM model_prices`)
	if err != nil {
		return 0, err
	}
	for priceRows.Next() {
		var p price
		if err = priceRows.Scan(&p.Model, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &p.LongInput, &p.LongOutput, &p.LongRead, &p.LongWrite, &p.FastInput, &p.FastOutput, &p.FastRead, &p.FastWrite, &p.Source, &p.UpdatedAt); err != nil {
			priceRows.Close()
			return 0, err
		}
		prices[normalizeModel(p.Model)] = p
	}
	if err = priceRows.Err(); err != nil {
		priceRows.Close()
		return 0, err
	}
	if err = priceRows.Close(); err != nil {
		return 0, err
	}

	eventRows, err := tx.QueryContext(ctx, `SELECT id,model,service_tier,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens FROM usage_events ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var events []costRecalculationEvent
	for eventRows.Next() {
		var event costRecalculationEvent
		if err = eventRows.Scan(&event.ID, &event.Model, &event.ServiceTier, &event.InputTokens, &event.OutputTokens, &event.CacheReadTokens, &event.CacheWriteTokens); err != nil {
			eventRows.Close()
			return 0, err
		}
		if !astraOnly || normalizeModel(event.Model) == "gpt-6-astra" {
			events = append(events, event)
		}
	}
	if err = eventRows.Err(); err != nil {
		eventRows.Close()
		return 0, err
	}
	if err = eventRows.Close(); err != nil {
		return 0, err
	}

	updateEvent, err := tx.PrepareContext(ctx, `UPDATE usage_events SET cost_usd=? WHERE id=?`)
	if err != nil {
		return 0, err
	}
	defer updateEvent.Close()
	for _, event := range events {
		cost := float64(0)
		if p, ok := prices[normalizeModel(event.Model)]; ok {
			cost = calculateCost(p, usageDetail{
				InputTokens:         event.InputTokens,
				OutputTokens:        event.OutputTokens,
				CacheReadTokens:     event.CacheReadTokens,
				CacheCreationTokens: event.CacheWriteTokens,
			}, event.ServiceTier, cfg)
		}
		if _, err = updateEvent.ExecContext(ctx, cost, event.ID); err != nil {
			return 0, err
		}
	}
	if err = updateEvent.Close(); err != nil {
		return 0, err
	}

	sampleSQL := `UPDATE quota_samples
SET window_cost_usd=COALESCE((
 SELECT SUM(usage_events.cost_usd)
 FROM usage_events
 WHERE usage_events.cycle_id=quota_samples.cycle_id
   AND usage_events.quota_scope=?
   AND usage_events.requested_at<=quota_samples.sampled_at
),0)`
	if astraOnly {
		sampleSQL += ` WHERE cycle_id IN (SELECT DISTINCT cycle_id FROM usage_events WHERE lower(trim(model))='gpt-6-astra' OR lower(trim(model)) LIKE '%/gpt-6-astra')`
	}
	if _, err = tx.ExecContext(ctx, sampleSQL, mainQuotaScope); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,'applied') ON CONFLICT(key) DO UPDATE SET value=excluded.value`, astraCalibrationKey); err != nil {
		return 0, err
	}

	raw, err := json.Marshal(settings)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, pricingSettingsMetadataKey, string(raw)); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(events)), nil
}
