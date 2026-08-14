package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type store struct{ db *sql.DB }

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=NORMAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON",
	} {
		if _, err = db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &store{db: db}
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS usage_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 requested_at INTEGER NOT NULL,
 account TEXT NOT NULL,
 provider TEXT NOT NULL DEFAULT '',
 model TEXT NOT NULL DEFAULT '',
 alias TEXT NOT NULL DEFAULT '',
 service_tier TEXT NOT NULL DEFAULT '',
 input_tokens INTEGER NOT NULL DEFAULT 0,
 output_tokens INTEGER NOT NULL DEFAULT 0,
 reasoning_tokens INTEGER NOT NULL DEFAULT 0,
 cache_read_tokens INTEGER NOT NULL DEFAULT 0,
 cache_write_tokens INTEGER NOT NULL DEFAULT 0,
 total_tokens INTEGER NOT NULL DEFAULT 0,
 cost_usd REAL NOT NULL DEFAULT 0,
 failed INTEGER NOT NULL DEFAULT 0,
 status_code INTEGER NOT NULL DEFAULT 0,
 used_percent REAL,
 reset_at INTEGER NOT NULL DEFAULT 0,
 window_minutes INTEGER NOT NULL DEFAULT 0,
 plan_type TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_usage_account_time ON usage_events(account, requested_at);
CREATE INDEX IF NOT EXISTS idx_usage_model_time ON usage_events(model, requested_at);
CREATE TABLE IF NOT EXISTS quota_samples (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 sampled_at INTEGER NOT NULL,
 account TEXT NOT NULL,
 used_percent REAL NOT NULL,
 reset_at INTEGER NOT NULL,
 window_minutes INTEGER NOT NULL,
 plan_type TEXT NOT NULL DEFAULT '',
 window_tokens INTEGER NOT NULL DEFAULT 0,
 window_cost_usd REAL NOT NULL DEFAULT 0,
 requests INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_quota_account_time ON quota_samples(account, sampled_at);
CREATE INDEX IF NOT EXISTS idx_quota_account_window ON quota_samples(account, reset_at, sampled_at);
CREATE TABLE IF NOT EXISTS model_prices (
 model TEXT PRIMARY KEY,
 input REAL NOT NULL DEFAULT 0,
 output REAL NOT NULL DEFAULT 0,
 cache_read REAL NOT NULL DEFAULT 0,
 cache_write REAL NOT NULL DEFAULT 0,
 long_input REAL NOT NULL DEFAULT 0,
 long_output REAL NOT NULL DEFAULT 0,
 long_cache_read REAL NOT NULL DEFAULT 0,
 long_cache_write REAL NOT NULL DEFAULT 0,
 fast_input REAL NOT NULL DEFAULT 0,
 fast_output REAL NOT NULL DEFAULT 0,
 fast_cache_read REAL NOT NULL DEFAULT 0,
 fast_cache_write REAL NOT NULL DEFAULT 0,
 source TEXT NOT NULL DEFAULT '',
 updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`)
	return err
}

func (s *store) close() error { return s.db.Close() }

func (s *store) insertEvent(ctx context.Context, e event, sampleInterval time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var used any
	if e.UsedPercent != nil {
		used = *e.UsedPercent
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_events
(requested_at,account,provider,model,alias,service_tier,input_tokens,output_tokens,reasoning_tokens,cache_read_tokens,cache_write_tokens,total_tokens,cost_usd,failed,status_code,used_percent,reset_at,window_minutes,plan_type)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.RequestedAt, e.Account, e.Provider, e.Model, e.Alias, e.ServiceTier,
		e.InputTokens, e.OutputTokens, e.ReasoningTokens, e.CacheReadTokens, e.CacheWriteTokens,
		e.TotalTokens, e.CostUSD, e.Failed, e.StatusCode, used, e.ResetAt, e.WindowMinutes, e.PlanType)
	if err != nil {
		return err
	}
	if e.UsedPercent != nil && e.ResetAt > 0 && e.WindowMinutes > 0 {
		var lastAt int64
		var lastPercent float64
		errLast := tx.QueryRowContext(ctx, `SELECT sampled_at, used_percent FROM quota_samples WHERE account=? AND reset_at=? ORDER BY sampled_at DESC LIMIT 1`, e.Account, e.ResetAt).Scan(&lastAt, &lastPercent)
		due := errLast == sql.ErrNoRows || lastPercent != *e.UsedPercent || e.RequestedAt-lastAt >= int64(sampleInterval/time.Second)
		if errLast != nil && errLast != sql.ErrNoRows {
			return errLast
		}
		if due {
			windowStart := e.ResetAt - e.WindowMinutes*60
			var tokens, requests int64
			var cost float64
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*) FROM usage_events WHERE account=? AND requested_at>=? AND requested_at<=?`, e.Account, windowStart, e.RequestedAt).Scan(&tokens, &cost, &requests); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO quota_samples(sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,window_cost_usd,requests) VALUES(?,?,?,?,?,?,?,?,?)`, e.RequestedAt, e.Account, *e.UsedPercent, e.ResetAt, e.WindowMinutes, e.PlanType, tokens, cost, requests)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *store) upsertPrices(ctx context.Context, prices []price) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO model_prices(model,input,output,cache_read,cache_write,long_input,long_output,long_cache_read,long_cache_write,fast_input,fast_output,fast_cache_read,fast_cache_write,source,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(model) DO UPDATE SET input=excluded.input,output=excluded.output,cache_read=excluded.cache_read,cache_write=excluded.cache_write,long_input=excluded.long_input,long_output=excluded.long_output,long_cache_read=excluded.long_cache_read,long_cache_write=excluded.long_cache_write,fast_input=excluded.fast_input,fast_output=excluded.fast_output,fast_cache_read=excluded.fast_cache_read,fast_cache_write=excluded.fast_cache_write,source=excluded.source,updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, p := range prices {
		_, err = stmt.ExecContext(ctx, p.Model, p.Input, p.Output, p.CacheRead, p.CacheWrite, p.LongInput, p.LongOutput, p.LongRead, p.LongWrite, p.FastInput, p.FastOutput, p.FastRead, p.FastWrite, p.Source, p.UpdatedAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *store) getPrice(ctx context.Context, model string) (price, bool, error) {
	model = normalizeModel(model)
	var p price
	err := s.db.QueryRowContext(ctx, `SELECT model,input,output,cache_read,cache_write,long_input,long_output,long_cache_read,long_cache_write,fast_input,fast_output,fast_cache_read,fast_cache_write,source,updated_at FROM model_prices WHERE lower(model)=? LIMIT 1`, model).Scan(
		&p.Model, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &p.LongInput, &p.LongOutput, &p.LongRead, &p.LongWrite, &p.FastInput, &p.FastOutput, &p.FastRead, &p.FastWrite, &p.Source, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *store) listPrices(ctx context.Context) ([]price, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT model,input,output,cache_read,cache_write,long_input,long_output,long_cache_read,long_cache_write,fast_input,fast_output,fast_cache_read,fast_cache_write,source,updated_at FROM model_prices ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []price
	for rows.Next() {
		var p price
		if err = rows.Scan(&p.Model, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &p.LongInput, &p.LongOutput, &p.LongRead, &p.LongWrite, &p.FastInput, &p.FastOutput, &p.FastRead, &p.FastWrite, &p.Source, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *store) accounts(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT account FROM quota_samples ORDER BY account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var x string
		if err = rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *store) latestPoints(ctx context.Context, account string, limit int) ([]quotaPoint, string, error) {
	if limit <= 0 || limit > 10000 {
		limit = 2000
	}
	var reset int64
	var plan string
	err := s.db.QueryRowContext(ctx, `SELECT reset_at,plan_type FROM quota_samples WHERE account=? ORDER BY sampled_at DESC LIMIT 1`, account).Scan(&reset, &plan)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sampled_at,used_percent,reset_at,window_minutes,window_tokens,window_cost_usd,requests FROM quota_samples WHERE account=? AND reset_at=? ORDER BY sampled_at ASC LIMIT ?`, account, reset, limit)
	if err != nil {
		return nil, "", err
	}
	var points []quotaPoint
	for rows.Next() {
		var p quotaPoint
		if err = rows.Scan(&p.Time, &p.UsedPercent, &p.ResetAt, &p.WindowMinutes, &p.WindowTokens, &p.WindowCostUSD, &p.Requests); err != nil {
			return nil, "", err
		}
		points = append(points, p)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, "", err
	}
	if err = rows.Close(); err != nil {
		return nil, "", err
	}
	// Quota samples are intentionally throttled. Append a live aggregate so the
	// cards never lag behind the most recent request by the sampling interval.
	if len(points) > 0 {
		last := points[len(points)-1]
		windowStart := last.ResetAt - last.WindowMinutes*60
		var live quotaPoint
		live.UsedPercent, live.ResetAt, live.WindowMinutes = last.UsedPercent, last.ResetAt, last.WindowMinutes
		err = s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(requested_at),0),COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),COUNT(*) FROM usage_events WHERE account=? AND requested_at>=?`, account, windowStart).Scan(&live.Time, &live.WindowTokens, &live.WindowCostUSD, &live.Requests)
		if err != nil {
			return nil, "", err
		}
		if live.Time > last.Time {
			points = append(points, live)
		}
	}
	return points, plan, nil
}

func (s *store) cleanup(ctx context.Context, days int) error {
	if days < 7 {
		days = 7
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	_, err := s.db.ExecContext(ctx, `DELETE FROM usage_events WHERE requested_at<?; DELETE FROM quota_samples WHERE sampled_at<?`, cutoff, cutoff)
	return err
}

func normalizeModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return m
}
