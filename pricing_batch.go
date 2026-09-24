package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"time"
)

const pricingEventBatchSize = 500
const pricingSampleBatchSize = 500
const pricingBatchPause = 2 * time.Millisecond

type repricedEvent struct {
	ID          int64
	CycleID     int64
	RequestedAt int64
	Scope       string
	NewCost     float64
	OldCost     float64
}

type pricingSnapshot struct {
	Events      []repricedEvent
	OldSamples  []sampleCostTarget
	CycleIDs    []int64
	MaxEventID  int64
	MaxSampleID int64
	SampleCount int64
}

type sampleCostTarget struct {
	ID, CycleID, At int64
	Cost            float64
}
type costPrefixEvent struct {
	At   int64
	Cost float64
}

func (s *store) openReadOnly() (*sql.DB, error) {
	if s.path == "" {
		return nil, fmt.Errorf("store path is required for read-only pricing snapshot")
	}
	absolute, err := filepath.Abs(s.path)
	if err != nil {
		return nil, err
	}
	uriPath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" && uriPath[0] != '/' {
		// SQLite requires /C:/... for an absolute Windows drive path.
		uriPath = "/" + uriPath
	}
	uri := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("initialize read-only SQLite connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err = db.Exec(`PRAGMA query_only=ON; PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize read-only SQLite connection: %w", err)
	}
	return db, nil
}

func readPricesInTx(ctx context.Context, tx *sql.Tx) (map[string]price, error) {
	rows, err := tx.QueryContext(ctx, `SELECT model,input,output,cache_read,cache_write,long_input,long_output,long_cache_read,long_cache_write,fast_input,fast_output,fast_cache_read,fast_cache_write,source,updated_at FROM model_prices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prices := make(map[string]price)
	for rows.Next() {
		var p price
		if err = rows.Scan(&p.Model, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &p.LongInput, &p.LongOutput, &p.LongRead, &p.LongWrite, &p.FastInput, &p.FastOutput, &p.FastRead, &p.FastWrite, &p.Source, &p.UpdatedAt); err != nil {
			return nil, err
		}
		prices[normalizeModel(p.Model)] = p
	}
	return prices, rows.Err()
}

func (s *store) preparePricingSnapshot(ctx context.Context, cfg config) (pricingSnapshot, error) {
	reader, err := s.openReadOnly()
	if err != nil {
		return pricingSnapshot{}, err
	}
	defer reader.Close()
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return pricingSnapshot{}, err
	}
	defer tx.Rollback()
	prices, err := readPricesInTx(ctx, tx)
	if err != nil {
		return pricingSnapshot{}, err
	}
	cfg.PriceCatalog = prices
	snapshot := pricingSnapshot{}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM usage_events`).Scan(&snapshot.MaxEventID); err != nil {
		return snapshot, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0),COUNT(*) FROM quota_samples`).Scan(&snapshot.MaxSampleID, &snapshot.SampleCount); err != nil {
		return snapshot, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,cycle_id,requested_at,quota_scope,model,service_tier,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_usd
  FROM usage_events WHERE id<=? ORDER BY cycle_id,requested_at,id`, snapshot.MaxEventID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Events = make([]repricedEvent, 0, 10000)
	for rows.Next() {
		var e repricedEvent
		var model, tier string
		var input, output, read, write int64
		if err = rows.Scan(&e.ID, &e.CycleID, &e.RequestedAt, &e.Scope, &model, &tier, &input, &output, &read, &write, &e.OldCost); err != nil {
			rows.Close()
			return snapshot, err
		}
		if p, ok := prices[normalizeModel(model)]; ok {
			e.NewCost = calculateCost(p, usageDetail{InputTokens: input, OutputTokens: output, CacheReadTokens: read, CacheCreationTokens: write}, tier, cfg)
		}
		snapshot.Events = append(snapshot.Events, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return snapshot, err
	}
	rows.Close()
	oldRows, err := tx.QueryContext(ctx, `SELECT id,cycle_id,sampled_at,window_cost_usd FROM quota_samples ORDER BY id`)
	if err != nil {
		return snapshot, err
	}
	for oldRows.Next() {
		var item sampleCostTarget
		if err = oldRows.Scan(&item.ID, &item.CycleID, &item.At, &item.Cost); err != nil {
			oldRows.Close()
			return snapshot, err
		}
		snapshot.OldSamples = append(snapshot.OldSamples, item)
	}
	if err = oldRows.Err(); err != nil {
		oldRows.Close()
		return snapshot, err
	}
	oldRows.Close()
	cycles, err := tx.QueryContext(ctx, `SELECT DISTINCT cycle_id FROM quota_samples ORDER BY cycle_id`)
	if err != nil {
		return snapshot, err
	}
	for cycles.Next() {
		var id int64
		if err = cycles.Scan(&id); err != nil {
			cycles.Close()
			return snapshot, err
		}
		snapshot.CycleIDs = append(snapshot.CycleIDs, id)
	}
	if err = cycles.Err(); err != nil {
		cycles.Close()
		return snapshot, err
	}
	cycles.Close()
	if err = tx.Commit(); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (s *store) writeEventBatch(ctx context.Context, events []repricedEvent, restore bool) (time.Duration, error) {
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE usage_events SET cost_usd=? WHERE id=?`)
	if err != nil {
		return 0, err
	}
	for _, e := range events {
		cost := e.NewCost
		if restore {
			cost = e.OldCost
		}
		if _, err = stmt.ExecContext(ctx, cost, e.ID); err != nil {
			stmt.Close()
			return 0, err
		}
	}
	if err = stmt.Close(); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return time.Since(started), nil
}

func (s *store) writeSampleBatch(ctx context.Context, samples []sampleCostTarget) (time.Duration, error) {
	started := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE quota_samples SET window_cost_usd=? WHERE id=?`)
	if err != nil {
		return 0, err
	}
	for _, sample := range samples {
		if _, err = stmt.ExecContext(ctx, sample.Cost, sample.ID); err != nil {
			stmt.Close()
			return 0, err
		}
	}
	if err = stmt.Close(); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return time.Since(started), nil
}

func (s *store) cycleSampleValues(ctx context.Context, reader *sql.DB, cycleID int64) ([]sampleCostTarget, error) {
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT requested_at,cost_usd FROM usage_events WHERE cycle_id=? AND quota_scope=? ORDER BY requested_at,id`, cycleID, mainQuotaScope)
	if err != nil {
		return nil, err
	}
	events := make([]costPrefixEvent, 0, 512)
	for rows.Next() {
		var e costPrefixEvent
		if err = rows.Scan(&e.At, &e.Cost); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = tx.QueryContext(ctx, `SELECT id,sampled_at FROM quota_samples WHERE cycle_id=? ORDER BY sampled_at,id`, cycleID)
	if err != nil {
		return nil, err
	}
	samples := make([]sampleCostTarget, 0, 128)
	index := 0
	running := float64(0)
	for rows.Next() {
		var item sampleCostTarget
		item.CycleID = cycleID
		if err = rows.Scan(&item.ID, &item.At); err != nil {
			rows.Close()
			return nil, err
		}
		for index < len(events) && events[index].At <= item.At {
			running += events[index].Cost
			index++
		}
		item.Cost = running
		samples = append(samples, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return samples, nil
}

func (s *store) writeCycleSamples(ctx context.Context, reader *sql.DB, cycleID int64, onBatch func(int, time.Duration) error) error {
	samples, err := s.cycleSampleValues(ctx, reader, cycleID)
	if err != nil {
		return err
	}
	for start := 0; start < len(samples); start += pricingSampleBatchSize {
		end := min(start+pricingSampleBatchSize, len(samples))
		held, writeErr := s.writeSampleBatch(ctx, samples[start:end])
		if writeErr != nil {
			return writeErr
		}
		if onBatch != nil {
			if err = onBatch(end-start, held); err != nil {
				return err
			}
		}
		if end < len(samples) {
			time.Sleep(pricingBatchPause)
		}
	}
	return nil
}

func (s *store) newCycleIDsSince(ctx context.Context, eventID, sampleID int64) ([]int64, error) {
	reader, err := s.openReadOnly()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	rows, err := reader.QueryContext(ctx, `SELECT cycle_id FROM usage_events WHERE id>? AND quota_scope=? UNION SELECT cycle_id FROM quota_samples WHERE id>? ORDER BY cycle_id`, eventID, mainQuotaScope, sampleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *store) persistPricingSettings(ctx context.Context, settings pricingSettings) error {
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, pricingSettingsMetadataKey, string(raw)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,'applied') ON CONFLICT(key) DO UPDATE SET value='applied'`, pricingFormulaMetadataKey); err != nil {
		return err
	}
	if settings.AutoFitAt > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fitRepriceMetadataKey, fmt.Sprint(settings.AutoFitAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) restorePricingSnapshot(ctx context.Context, plan pricingSnapshot, oldCfg config) error {
	// The active config is switched back by the caller before this function.
	for start := 0; start < len(plan.Events); start += pricingEventBatchSize {
		end := min(start+pricingEventBatchSize, len(plan.Events))
		if _, err := s.writeEventBatch(ctx, plan.Events[start:end], true); err != nil {
			return err
		}
		if end < len(plan.Events) {
			time.Sleep(pricingBatchPause)
		}
	}
	// Requests accepted after the read snapshot used the new config. Reprice
	// those to the restored config without disturbing their raw Token records.
	reader, err := s.openReadOnly()
	if err != nil {
		return err
	}
	tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		reader.Close()
		return err
	}
	prices, err := readPricesInTx(ctx, tx)
	if err != nil {
		tx.Rollback()
		reader.Close()
		return err
	}
	oldCfg.PriceCatalog = prices
	rows, err := tx.QueryContext(ctx, `SELECT id,cycle_id,requested_at,quota_scope,model,service_tier,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,cost_usd FROM usage_events WHERE id>? ORDER BY id`, plan.MaxEventID)
	if err != nil {
		tx.Rollback()
		reader.Close()
		return err
	}
	var late []repricedEvent
	for rows.Next() {
		var e repricedEvent
		var model, tier string
		var input, output, read, write int64
		if err = rows.Scan(&e.ID, &e.CycleID, &e.RequestedAt, &e.Scope, &model, &tier, &input, &output, &read, &write, &e.NewCost); err != nil {
			rows.Close()
			tx.Rollback()
			reader.Close()
			return err
		}
		if p, ok := prices[normalizeModel(model)]; ok {
			e.OldCost = calculateCost(p, usageDetail{InputTokens: input, OutputTokens: output, CacheReadTokens: read, CacheCreationTokens: write}, tier, oldCfg)
		}
		late = append(late, e)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		tx.Rollback()
		reader.Close()
		return err
	}
	rows.Close()
	tx.Commit()
	reader.Close()
	for start := 0; start < len(late); start += pricingEventBatchSize {
		end := min(start+pricingEventBatchSize, len(late))
		if _, err = s.writeEventBatch(ctx, late[start:end], true); err != nil {
			return err
		}
		if end < len(late) {
			time.Sleep(pricingBatchPause)
		}
	}
	lateByCycle := make(map[int64][]costPrefixEvent)
	for _, e := range late {
		if e.Scope == mainQuotaScope {
			lateByCycle[e.CycleID] = append(lateByCycle[e.CycleID], costPrefixEvent{At: e.RequestedAt, Cost: e.OldCost})
		}
	}
	for cycleID, events := range lateByCycle {
		sort.Slice(events, func(i, j int) bool { return events[i].At < events[j].At })
		for i := 1; i < len(events); i++ {
			events[i].Cost += events[i-1].Cost
		}
		lateByCycle[cycleID] = events
	}
	restoredSamples := make([]sampleCostTarget, len(plan.OldSamples))
	for i, sample := range plan.OldSamples {
		if events := lateByCycle[sample.CycleID]; len(events) > 0 {
			index := sort.Search(len(events), func(j int) bool { return events[j].At > sample.At })
			if index > 0 {
				sample.Cost += events[index-1].Cost
			}
		}
		restoredSamples[i] = sample
	}
	for start := 0; start < len(restoredSamples); start += pricingSampleBatchSize {
		end := min(start+pricingSampleBatchSize, len(restoredSamples))
		if _, err = s.writeSampleBatch(ctx, restoredSamples[start:end]); err != nil {
			return err
		}
		if end < len(restoredSamples) {
			time.Sleep(pricingBatchPause)
		}
	}
	reader, err = s.openReadOnly()
	if err != nil {
		return err
	}
	defer reader.Close()
	cycles, err := reader.QueryContext(ctx, `SELECT DISTINCT cycle_id FROM quota_samples WHERE id>? ORDER BY cycle_id`, plan.MaxSampleID)
	if err != nil {
		return err
	}
	var ids []int64
	for cycles.Next() {
		var id int64
		if err = cycles.Scan(&id); err != nil {
			cycles.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err = cycles.Err(); err != nil {
		cycles.Close()
		return err
	}
	cycles.Close()
	for _, id := range ids {
		values, readErr := s.cycleSampleValues(ctx, reader, id)
		if readErr != nil {
			return readErr
		}
		var fresh []sampleCostTarget
		for _, item := range values {
			if item.ID > plan.MaxSampleID {
				fresh = append(fresh, item)
			}
		}
		for start := 0; start < len(fresh); start += pricingSampleBatchSize {
			end := min(start+pricingSampleBatchSize, len(fresh))
			if _, err = s.writeSampleBatch(ctx, fresh[start:end]); err != nil {
				return err
			}
			if end < len(fresh) {
				time.Sleep(pricingBatchPause)
			}
		}
	}
	return nil
}

func (s *store) recalculatePricingBatches(ctx context.Context, settings pricingSettings, newCfg, oldCfg config, progress func(pricingRecalcProgress) error, onRollback func()) (int64, error) {
	var err error
	settings, err = normalizePricingSettings(settings)
	if err != nil {
		return 0, err
	}
	newCfg = newCfg.withPricingSettings(settings)
	plan, err := s.preparePricingSnapshot(ctx, newCfg)
	if err != nil {
		return 0, err
	}
	state := pricingRecalcProgress{Stage: "events", EventsTotal: int64(len(plan.Events)), SamplesTotal: plan.SampleCount}
	report := func() error {
		if progress != nil {
			return progress(state)
		}
		return nil
	}
	if err = report(); err != nil {
		return 0, err
	}
	wrote := false
	recordBatch := func(count int, held time.Duration) error {
		wrote = true
		state.BatchCount++
		if ms := float64(held) / float64(time.Millisecond); ms > state.MaxBatchLockMS {
			state.MaxBatchLockMS = ms
		}
		if state.Stage == "events" {
			state.EventsDone += int64(count)
		} else {
			state.SamplesDone += int64(count)
		}
		return report()
	}
	for start := 0; start < len(plan.Events); start += pricingEventBatchSize {
		end := min(start+pricingEventBatchSize, len(plan.Events))
		held, writeErr := s.writeEventBatch(ctx, plan.Events[start:end], false)
		if writeErr != nil {
			err = writeErr
			break
		}
		if err = recordBatch(end-start, held); err != nil {
			break
		}
		if end < len(plan.Events) {
			time.Sleep(pricingBatchPause)
		}
	}
	if err == nil {
		state.Stage = "samples"
		err = report()
	}
	if err == nil {
		reader, readErr := s.openReadOnly()
		if readErr != nil {
			err = readErr
		} else {
			for _, cycleID := range plan.CycleIDs {
				if err = s.writeCycleSamples(ctx, reader, cycleID, recordBatch); err != nil {
					break
				}
			}
			reader.Close()
		}
	}
	if err == nil {
		state.Stage = "catch_up"
		err = report()
	}
	if err == nil {
		ids, findErr := s.newCycleIDsSince(ctx, plan.MaxEventID, plan.MaxSampleID)
		if findErr != nil {
			err = findErr
		} else if len(ids) > 0 {
			reader, readErr := s.openReadOnly()
			if readErr != nil {
				err = readErr
			} else {
				for _, cycleID := range ids {
					if err = s.writeCycleSamples(ctx, reader, cycleID, recordBatch); err != nil {
						break
					}
				}
				reader.Close()
			}
		}
	}
	if err == nil {
		err = s.persistPricingSettings(ctx, settings)
	}
	if err != nil {
		if onRollback != nil {
			onRollback()
		}
		if wrote {
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			restoreErr := s.restorePricingSnapshot(rollbackCtx, plan, oldCfg)
			cancel()
			if restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("pricing rollback failed: %w", restoreErr))
			}
		}
		return 0, err
	}
	state.Stage = "done"
	_ = report()
	return int64(len(plan.Events)), nil
}
