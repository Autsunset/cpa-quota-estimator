package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"time"
)

func (s *store) latestWeightFit(ctx context.Context) (weightBacktest, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT backtest_json FROM weight_fits ORDER BY id DESC LIMIT 1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return weightBacktest{}, false, nil
	}
	if err != nil {
		return weightBacktest{}, false, err
	}
	var result weightBacktest
	if err = json.Unmarshal([]byte(raw), &result); err != nil {
		return weightBacktest{}, false, err
	}
	return result, true, nil
}

func (s *store) refreshWeightFit(ctx context.Context, opts weightLearnerOptions) (weightBacktest, error) {
	byLag := make(map[int][]quotaSegment)
	var err error
	for lag := 0; lag <= 2; lag++ {
		byLag[lag], err = s.quotaSegments(ctx, lag)
		if err != nil {
			return weightBacktest{}, err
		}
	}
	priceRows, err := s.listPrices(ctx)
	if err != nil {
		return weightBacktest{}, err
	}
	prices := make(map[string]price, len(priceRows))
	for _, p := range priceRows {
		prices[normalizeModel(p.Model)] = p
	}
	now := time.Now().Unix()
	result, err := rollingBacktest(byLag, prices, now, opts)
	if err != nil {
		return result, err
	}
	rawFit, err := json.Marshal(result.FittedWeights)
	if err != nil {
		return result, err
	}
	rawBacktest, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO weight_fits(fitted_at,lag,segment_count,fit_json,backtest_json) VALUES(?,?,?,?,?)`,
		now, result.SelectedLag, result.FittedWeights.SegmentCount, string(rawFit), string(rawBacktest))
	if err != nil {
		return result, err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM weight_fits WHERE id NOT IN (SELECT id FROM weight_fits ORDER BY id DESC LIMIT 2160)`)
	return result, nil
}

func learnedEquivalentForUsage(fit *weightFit, model string, d usageDetail, serviceTier string, longThreshold int64) (float64, bool) {
	if fit == nil || !fit.Available {
		return 0, false
	}
	model = normalizeModel(model)
	var row *learnedModelWeights
	for i := range fit.Models {
		if fit.Models[i].Model == model {
			row = &fit.Models[i]
			break
		}
	}
	if row == nil {
		return 0, false
	}
	read := max(d.CacheReadTokens, d.CachedTokens)
	uncached := d.InputTokens - read - d.CacheCreationTokens
	if uncached < 0 {
		uncached = 0
	}
	input := uncached + d.CacheCreationTokens
	value := (float64(input)*row.Input.Value + float64(read)*row.Cache.Value + float64(d.OutputTokens)*row.Output.Value) / 1_000_000
	if isFastTier(serviceTier) {
		value *= fit.Fast.Value
	}
	if d.InputTokens > longThreshold {
		value *= fit.LongContext.Value
	}
	return value, true
}

func learnedScale(fit *weightFit, account, window string) (float64, bool) {
	if fit == nil || !fit.Available {
		return 0, false
	}
	want := "scale:" + account + "|" + window
	for i, name := range fit.ParameterNames {
		if name == want && i < len(fit.LogParameters) {
			return math.Exp(fit.LogParameters[i]), true
		}
	}
	return 0, false
}

func learnedQuotaAttribution(fit *weightFit, account, window, model, serviceTier string, d usageDetail, longThreshold int64) *float64 {
	value, ok := learnedEquivalentForUsage(fit, model, d, serviceTier, longThreshold)
	if !ok {
		return nil
	}
	scale, ok := learnedScale(fit, account, window)
	if !ok {
		return nil
	}
	result := value * scale
	return &result
}

type attributionEvent struct {
	ID           int64
	Account      string
	Scope        string
	Model        string
	Tier         string
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite   int64
	HasSecondary bool
	Failed       bool
}

func (s *store) updateWeightAttributions(ctx context.Context, fit *weightFit, longThreshold int64) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,account,quota_scope,model,service_tier,
		input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,
		CASE WHEN secondary_used_percent IS NOT NULL THEN 1 ELSE 0 END,failed FROM usage_events ORDER BY id`)
	if err != nil {
		return 0, err
	}
	events := make([]attributionEvent, 0, 10000)
	for rows.Next() {
		var e attributionEvent
		if err = rows.Scan(&e.ID, &e.Account, &e.Scope, &e.Model, &e.Tier,
			&e.Input, &e.Output, &e.CacheRead, &e.CacheWrite, &e.HasSecondary, &e.Failed); err != nil {
			rows.Close()
			return 0, err
		}
		events = append(events, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE usage_events SET learned_quota_pct=?,learned_secondary_pct=? WHERE id=?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, e := range events {
		detail := usageDetail{InputTokens: e.Input, OutputTokens: e.Output, CacheReadTokens: e.CacheRead, CacheCreationTokens: e.CacheWrite}
		var primary *float64
		var secondary *float64
		if !e.Failed {
			primary = learnedQuotaAttribution(fit, e.Account, e.Scope, e.Model, e.Tier, detail, longThreshold)
		}
		if e.HasSecondary && !e.Failed {
			window := weeklyQuotaScope
			if e.Scope == sparkQuotaScope {
				window = sparkWeeklyQuotaScope
			}
			secondary = learnedQuotaAttribution(fit, e.Account, window, e.Model, e.Tier, detail, longThreshold)
		}
		if _, err = stmt.ExecContext(ctx, primary, secondary, e.ID); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(events)), nil
}
