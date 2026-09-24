package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strings"
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
	return s.saveWeightFit(ctx, result, now)
}

// The hourly path reuses the latest selected lag and scores. A full rolling
// backtest is still run daily and whenever calibration explicitly requests it.
func (s *store) refreshWeightFitScheduled(ctx context.Context, opts weightLearnerOptions, previous weightBacktest, hasPrevious bool) (weightBacktest, error) {
	now := time.Now().Unix()
	if !hasPrevious || previous.GeneratedAt <= 0 || now-previous.GeneratedAt >= 24*3600 ||
		previous.FittedWeights.RandomWalkSigma != opts.RandomWalkSigma || previous.FittedWeights.HalfLifeDays != opts.HalfLifeDays {
		return s.refreshWeightFit(ctx, opts)
	}
	segments, err := s.quotaSegments(ctx, previous.SelectedLag)
	if err != nil {
		return weightBacktest{}, err
	}
	priceRows, err := s.listPrices(ctx)
	if err != nil {
		return weightBacktest{}, err
	}
	prices := make(map[string]price, len(priceRows))
	for _, p := range priceRows {
		prices[normalizeModel(p.Model)] = p
	}
	fit, err := fitQuotaWeights(segments, prices, now, opts)
	if err != nil {
		return weightBacktest{}, err
	}
	fit.Lag = previous.SelectedLag
	previous.FittedWeights = fit
	return s.saveWeightFit(ctx, previous, now)
}

func (s *store) saveWeightFit(ctx context.Context, result weightBacktest, fittedAt int64) (weightBacktest, error) {
	rawFit, err := json.Marshal(result.FittedWeights)
	if err != nil {
		return result, err
	}
	rawBacktest, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO weight_fits(fitted_at,lag,segment_count,fit_json,backtest_json) VALUES(?,?,?,?,?)`,
		fittedAt, result.SelectedLag, result.FittedWeights.SegmentCount, string(rawFit), string(rawBacktest))
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

func learnedScale(fit *weightFit, account, window string, cycleID, resetAt int64) (float64, bool) {
	if fit == nil || !fit.Available {
		return 0, false
	}
	var latest *learnedCycleScale
	for i := range fit.CycleScales {
		cycle := &fit.CycleScales[i]
		if cycle.Account != account || cycle.Window != window {
			continue
		}
		if cycleID > 0 && cycle.CycleID == cycleID && absSegmentTime(cycle.RegimeResetAt-resetAt) <= segmentResetSlack {
			return cycle.Scale.Value, true
		}
		if resetAt > 0 && absSegmentTime(cycle.RegimeResetAt-resetAt) <= segmentResetSlack {
			latest = cycle
			continue
		}
		if latest == nil || cycle.StartedAt > latest.StartedAt {
			latest = cycle
		}
	}
	if latest != nil {
		return latest.Scale.Value, true
	}
	// Older persisted fits did not include the derived cycle-scale list.
	wantPrefix := "scale:" + account + "|" + window + "|"
	var value float64
	for i, name := range fit.ParameterNames {
		if strings.HasPrefix(name, wantPrefix) && i < len(fit.LogParameters) {
			value = math.Exp(fit.LogParameters[i])
		}
	}
	if value > 0 {
		return value, true
	}
	return 0, false
}

func learnedQuotaAttribution(fit *weightFit, account, window, model, serviceTier string, cycleID, resetAt int64, d usageDetail, longThreshold int64) *float64 {
	value, ok := learnedEquivalentForUsage(fit, model, d, serviceTier, longThreshold)
	if !ok {
		return nil
	}
	scale, ok := learnedScale(fit, account, window, cycleID, resetAt)
	if !ok {
		return nil
	}
	result := value * scale
	return &result
}

type attributionEvent struct {
	ID               int64
	CycleID          int64
	ResetAt          int64
	SecondaryResetAt int64
	Account          string
	Scope            string
	Model            string
	Tier             string
	Input            int64
	Output           int64
	CacheRead        int64
	CacheWrite       int64
	HasSecondary     bool
	Failed           bool
}

type attributionUpdate struct {
	ID                 int64
	Primary, Secondary *float64
}

func (s *store) updateWeightAttributions(ctx context.Context, fit *weightFit, longThreshold int64) (int64, error) {
	reader, err := s.openReadOnly()
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	rows, err := reader.QueryContext(ctx, `SELECT id,cycle_id,reset_at,secondary_reset_at,account,quota_scope,model,service_tier,
  input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,
  CASE WHEN secondary_used_percent IS NOT NULL THEN 1 ELSE 0 END,failed FROM usage_events ORDER BY id`)
	if err != nil {
		return 0, err
	}
	updates := make([]attributionUpdate, 0, 10000)
	for rows.Next() {
		var e attributionEvent
		if err = rows.Scan(&e.ID, &e.CycleID, &e.ResetAt, &e.SecondaryResetAt, &e.Account, &e.Scope, &e.Model, &e.Tier, &e.Input, &e.Output, &e.CacheRead, &e.CacheWrite, &e.HasSecondary, &e.Failed); err != nil {
			rows.Close()
			return 0, err
		}
		detail := usageDetail{InputTokens: e.Input, OutputTokens: e.Output, CacheReadTokens: e.CacheRead, CacheCreationTokens: e.CacheWrite}
		update := attributionUpdate{ID: e.ID}
		if !e.Failed {
			update.Primary = learnedQuotaAttribution(fit, e.Account, e.Scope, e.Model, e.Tier, e.CycleID, e.ResetAt, detail, longThreshold)
		}
		if e.HasSecondary && !e.Failed {
			window := weeklyQuotaScope
			if e.Scope == sparkQuotaScope {
				window = sparkWeeklyQuotaScope
			}
			update.Secondary = learnedQuotaAttribution(fit, e.Account, window, e.Model, e.Tier, e.SecondaryResetAt, e.SecondaryResetAt, detail, longThreshold)
		}
		updates = append(updates, update)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	for start := 0; start < len(updates); start += pricingEventBatchSize {
		end := min(start+pricingEventBatchSize, len(updates))
		batchStarted := time.Now()
		tx, txErr := s.db.BeginTx(ctx, nil)
		if txErr != nil {
			return int64(start), txErr
		}
		stmt, prepErr := tx.PrepareContext(ctx, `UPDATE usage_events SET learned_quota_pct=?,learned_secondary_pct=? WHERE id=?`)
		if prepErr != nil {
			tx.Rollback()
			return int64(start), prepErr
		}
		for _, update := range updates[start:end] {
			if _, err = stmt.ExecContext(ctx, update.Primary, update.Secondary, update.ID); err != nil {
				stmt.Close()
				tx.Rollback()
				return int64(start), err
			}
		}
		if err = stmt.Close(); err != nil {
			tx.Rollback()
			return int64(start), err
		}
		if err = tx.Commit(); err != nil {
			return int64(start), err
		}
		held := time.Since(batchStarted).Nanoseconds()
		for {
			old := s.attributionBatchMaxNS.Load()
			if held <= old || s.attributionBatchMaxNS.CompareAndSwap(old, held) {
				break
			}
		}
		if end < len(updates) {
			time.Sleep(pricingBatchPause)
		}
	}
	go func() {
		checkpointCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.checkpointWAL(checkpointCtx)
	}()
	return int64(len(updates)), nil
}
