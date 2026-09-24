package main

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"time"
)

const fitRepriceMetadataKey = "last_fit_pricing_recalculation_at"
const fitRepriceMinInterval = int64(6 * time.Hour / time.Second)
const fitRepriceMinChange = .02

func (s *store) lastFitRepriceAt(ctx context.Context) (int64, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, fitRepriceMetadataKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(raw, 10, 64)
}

func (s *store) saveFitRepriceAt(ctx context.Context, at int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fitRepriceMetadataKey, strconv.FormatInt(at, 10))
	return err
}

func relativeFactorChanged(old, new float64) bool {
	if old <= 0 {
		return new > 0
	}
	return math.Abs(new/old-1) > fitRepriceMinChange
}

func fitRepricingDue(priced *weightFit, cfg config, lastAt, now int64) bool {
	if normalizePricingMode(cfg.PricingMode) == pricingModeCustom || cfg.LearnedFit == nil || !cfg.LearnedFit.Available {
		return false
	}
	if lastAt > 0 && now-lastAt < fitRepriceMinInterval {
		return false
	}
	previous := cfg
	previous.LearnedFit = priced
	if relativeFactorChanged(previous.effectiveLongMultiplier(), cfg.effectiveLongMultiplier()) {
		return true
	}
	for _, p := range cfg.PriceCatalog {
		if relativeFactorChanged(previous.modelPriceAdjustment(p).Factor, cfg.modelPriceAdjustment(p).Factor) {
			return true
		}
		if relativeFactorChanged(previous.effectiveFastMultiplier(p), cfg.effectiveFastMultiplier(p)) {
			return true
		}
	}
	return false
}

func (a *app) queueFitRepricing(s *store, cfg config) {
	a.pricingTaskMu.Lock()
	priced, last := a.lastPricedFit, a.lastAutoFitRepriceAt
	a.pricingTaskMu.Unlock()
	now := time.Now().Unix()
	if !fitRepricingDue(priced, cfg, last, now) {
		return
	}
	settings := cfg.pricingSettings()
	settings.AutoFitAt = now
	if _, err := a.startPricingTask(s, settings, cfg); err != nil {
		a.pricingTaskMu.Lock()
		a.pricingFitPending = true
		a.pricingTaskMu.Unlock()
		return
	}
	a.pricingTaskMu.Lock()
	a.lastAutoFitRepriceAt = now
	a.pricingTaskMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = s.saveFitRepriceAt(ctx, now)
	cancel()
}
