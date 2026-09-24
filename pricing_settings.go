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
	Stage        string `json:"stage"`
	EventsDone   int64  `json:"events_done"`
	EventsTotal  int64  `json:"events_total"`
	SamplesDone  int64  `json:"samples_done"`
	SamplesTotal int64  `json:"samples_total"`
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

type costRecalculationEvent struct {
	ID               int64
	CycleID          int64
	RequestedAt      int64
	Scope            string
	Model            string
	ServiceTier      string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

type costPrefixEvent struct {
	At   int64
	Cost float64
}
type sampleCostTarget struct{ ID, CycleID, At int64 }

// recalculatePricing reprices requests and rebuilds every cumulative sample
// in one transaction. The caller may observe progress in memory; an error from
// the callback rolls back both request values and saved settings.
func (s *store) recalculatePricing(ctx context.Context, settings pricingSettings, cfg config, progress func(pricingRecalcProgress) error) (int64, error) {
	var err error
	settings, err = normalizePricingSettings(settings)
	if err != nil {
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
	priceRows.Close()
	cfg.PriceCatalog = prices
	rows, err := tx.QueryContext(ctx, `SELECT id,cycle_id,requested_at,quota_scope,model,service_tier,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens FROM usage_events ORDER BY cycle_id,requested_at,id`)
	if err != nil {
		return 0, err
	}
	events := make([]costRecalculationEvent, 0, 10000)
	for rows.Next() {
		var e costRecalculationEvent
		if err = rows.Scan(&e.ID, &e.CycleID, &e.RequestedAt, &e.Scope, &e.Model, &e.ServiceTier, &e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheWriteTokens); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	sampleRows, err := tx.QueryContext(ctx, `SELECT id,cycle_id,sampled_at FROM quota_samples ORDER BY cycle_id,sampled_at,id`)
	if err != nil {
		return 0, err
	}
	samples := make([]sampleCostTarget, 0, 1000)
	for sampleRows.Next() {
		var sample sampleCostTarget
		if err = sampleRows.Scan(&sample.ID, &sample.CycleID, &sample.At); err != nil {
			sampleRows.Close()
			return 0, err
		}
		samples = append(samples, sample)
	}
	if err = sampleRows.Err(); err != nil {
		sampleRows.Close()
		return 0, err
	}
	sampleRows.Close()
	state := pricingRecalcProgress{Stage: "events", EventsTotal: int64(len(events)), SamplesTotal: int64(len(samples))}
	if progress != nil {
		if err = progress(state); err != nil {
			return 0, err
		}
	}
	updateEvent, err := tx.PrepareContext(ctx, `UPDATE usage_events SET cost_usd=? WHERE id=?`)
	if err != nil {
		return 0, err
	}
	defer updateEvent.Close()
	byCycle := make(map[int64][]costPrefixEvent)
	for i, e := range events {
		cost := float64(0)
		if p, ok := prices[normalizeModel(e.Model)]; ok {
			cost = calculateCost(p, usageDetail{InputTokens: e.InputTokens, OutputTokens: e.OutputTokens, CacheReadTokens: e.CacheReadTokens, CacheCreationTokens: e.CacheWriteTokens}, e.ServiceTier, cfg)
		}
		if _, err = updateEvent.ExecContext(ctx, cost, e.ID); err != nil {
			return 0, err
		}
		if e.Scope == mainQuotaScope {
			byCycle[e.CycleID] = append(byCycle[e.CycleID], costPrefixEvent{At: e.RequestedAt, Cost: cost})
		}
		state.EventsDone = int64(i + 1)
		if progress != nil && (i%512 == 511 || i == len(events)-1) {
			if err = progress(state); err != nil {
				return 0, err
			}
		}
	}
	if err = updateEvent.Close(); err != nil {
		return 0, err
	}
	state.Stage = "samples"
	if progress != nil {
		if err = progress(state); err != nil {
			return 0, err
		}
	}
	updateSample, err := tx.PrepareContext(ctx, `UPDATE quota_samples SET window_cost_usd=? WHERE id=?`)
	if err != nil {
		return 0, err
	}
	defer updateSample.Close()
	var cycleID int64 = -1
	var eventsInCycle []costPrefixEvent
	index := 0
	running := float64(0)
	for i, sample := range samples {
		if cycleID != sample.CycleID {
			cycleID = sample.CycleID
			eventsInCycle = byCycle[cycleID]
			index = 0
			running = 0
		}
		for index < len(eventsInCycle) && eventsInCycle[index].At <= sample.At {
			running += eventsInCycle[index].Cost
			index++
		}
		if _, err = updateSample.ExecContext(ctx, running, sample.ID); err != nil {
			return 0, err
		}
		state.SamplesDone = int64(i + 1)
		if progress != nil && (i%256 == 255 || i == len(samples)-1) {
			if err = progress(state); err != nil {
				return 0, err
			}
		}
	}
	if err = updateSample.Close(); err != nil {
		return 0, err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, pricingSettingsMetadataKey, string(raw)); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,'applied') ON CONFLICT(key) DO UPDATE SET value='applied'`, pricingFormulaMetadataKey); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	state.Stage = "done"
	if progress != nil {
		_ = progress(state)
	}
	return int64(len(events)), nil
}

// Retained for in-process migrations and offline analysis. Management saves
// use the background task path.
func (s *store) savePricingSettingsAndRecalculate(ctx context.Context, settings pricingSettings, cfg config) (int64, error) {
	return s.recalculatePricing(ctx, settings, cfg, nil)
}
