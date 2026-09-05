package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type app struct {
	mu     sync.RWMutex
	cfg    config
	store  *store
	cancel context.CancelFunc
}

var globalApp = &app{cfg: defaultConfig()}

func (a *app) handle(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		req, err := decode[lifecycleRequest](raw)
		if err != nil {
			return nil, err
		}
		if err = a.configure(req.ConfigYAML); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case "usage.handle":
		record, err := decode[usageRecord](raw)
		if err != nil {
			return nil, err
		}
		if err = a.recordUsage(record); err != nil {
			return nil, err
		}
		return okEnvelope(struct{}{})
	case "management.register":
		return okEnvelope(managementRegistration())
	case "management.handle":
		req, err := decode[managementRequest](raw)
		if err != nil {
			return nil, err
		}
		resp := a.handleManagement(req)
		return okEnvelope(resp)
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func pluginRegistration() any {
	return map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name": pluginName, "Version": pluginVersion, "Author": "Autsunset", "GitHubRepository": "https://github.com/Autsunset/cpa-quota-estimator", "Logo": "https://raw.githubusercontent.com/Autsunset/cpa-quota-estimator/main/logo.png",
			"ConfigFields": []map[string]any{
				{"Name": "enabled", "Type": "boolean", "Description": "是否采集用量和额度样本"},
				{"Name": "data_path", "Type": "string", "Description": "SQLite 数据库路径"},
				{"Name": "sample_interval_minutes", "Type": "integer", "Description": "额度未变化时的最小采样间隔"},
				{"Name": "fast_pricing_mode", "Type": "enum", "EnumValues": []string{"multiplier", "source"}, "Description": "Fast 按倍率或价格源显式价格计费"},
				{"Name": "fast_multiplier", "Type": "number", "Description": "Fast 倍率，默认 2.5"},
				{"Name": "pricing_mode", "Type": "enum", "EnumValues": []string{pricingModeCurrentAPI, pricingModeLegacyAPI, pricingModeCredits}, "Description": "计价口径：优惠前 API（新用户默认）、当前 API 或 Credits"},
				{"Name": "astra_multiplier", "Type": "number", "Description": "Astra 额度倍率，默认 1.8，范围 0.01–100"},
				{"Name": "apply_fast_pricing", "Type": "boolean", "Description": "是否应用 Fast 加价，默认开启；仪表盘保存值会覆盖此初始值"},
				{"Name": "apply_long_context_pricing", "Type": "boolean", "Description": "是否应用长上下文加价，默认关闭；仪表盘保存值会覆盖此初始值"},
			},
		},
		"capabilities": map[string]any{"usage_plugin": true, "management_api": true},
	}
}

func managementRegistration() any {
	base := "/" + pluginID
	return map[string]any{
		"routes": []map[string]any{
			{"Method": "GET", "Path": base + "/overview"},
			{"Method": "GET", "Path": base + "/summary"},
			{"Method": "GET", "Path": base + "/series"},
			{"Method": "GET", "Path": base + "/monthly"},
			{"Method": "GET", "Path": base + "/repair/early-resets"},
			{"Method": "POST", "Path": base + "/repair/early-resets"},
			{"Method": "GET", "Path": base + "/prices"},
			{"Method": "POST", "Path": base + "/prices/sync"},
			{"Method": "GET", "Path": base + "/pricing-settings"},
			{"Method": "POST", "Path": base + "/pricing-settings"},
		},
		"resources": []map[string]any{{"Path": "/dashboard", "Menu": "额度容量预测", "Description": "按额度增长反推周期 Token 与美元等效容量，并绘制趋势曲线。"}},
	}
}

func parseConfig(raw []byte) (config, error) {
	cfg := defaultConfig()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse plugin config: %w", err)
		}
	}
	if err := validateAstraMultiplier(cfg.AstraMultiplier); err != nil {
		return cfg, err
	}
	if cfg.DataPath == "" {
		cfg.DataPath = defaultConfig().DataPath
	}
	if cfg.SampleIntervalMinutes < 1 {
		cfg.SampleIntervalMinutes = 5
	}
	if cfg.PriceSourceURL == "" {
		cfg.PriceSourceURL = defaultConfig().PriceSourceURL
	}
	if cfg.PriceSyncIntervalMinutes < 5 {
		cfg.PriceSyncIntervalMinutes = 1440
	}
	if cfg.FastPricingMode != "source" && cfg.FastPricingMode != "multiplier" {
		cfg.FastPricingMode = "multiplier"
	}
	if cfg.FastMultiplier <= 0 {
		cfg.FastMultiplier = 2.5
	}
	cfg.PricingMode = normalizePricingMode(cfg.PricingMode)
	if cfg.LongContextThreshold <= 0 {
		cfg.LongContextThreshold = 272000
	}
	if cfg.HistoryDays < 7 {
		cfg.HistoryDays = 365
	}
	return cfg, nil
}

func (a *app) configure(raw []byte) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil && a.cfg.DataPath == cfg.DataPath {
		settings, errLoad := a.store.loadPricingSettings(context.Background(), cfg.pricingSettings())
		if errLoad != nil {
			return errLoad
		}
		cfg = cfg.withPricingSettings(settings)
		a.cfg = cfg
		if a.cancel != nil {
			a.cancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		a.cancel = cancel
		go a.background(ctx, a.store, cfg)
		return nil
	}
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	if a.store != nil {
		_ = a.store.close()
		a.store = nil
	}
	s, err := openStore(cfg.DataPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	if err = seedPrices(context.Background(), s); err != nil {
		_ = s.close()
		return err
	}
	settings, err := s.loadPricingSettings(context.Background(), cfg.pricingSettings())
	if err != nil {
		_ = s.close()
		return err
	}
	cfg = cfg.withPricingSettings(settings)
	if err = s.ensureAstraCalibration(context.Background(), cfg); err != nil {
		_ = s.close()
		return fmt.Errorf("calibrate Astra history: %w", err)
	}
	a.cfg, a.store = cfg, s
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	go a.background(ctx, s, cfg)
	return nil
}

func (a *app) background(ctx context.Context, s *store, cfg config) {
	_, _ = syncPrices(ctx, s, cfg)
	_, _ = s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('last_price_sync_attempt',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatInt(time.Now().Unix(), 10))
	ticker := time.NewTicker(time.Duration(cfg.PriceSyncIntervalMinutes) * time.Minute)
	defer ticker.Stop()
	cleanup := time.NewTicker(24 * time.Hour)
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = syncPrices(ctx, s, cfg)
		case <-cleanup.C:
			_ = s.cleanup(ctx, cfg.HistoryDays)
		}
	}
}

func (a *app) shutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	if a.store != nil {
		_ = a.store.close()
	}
	a.cancel = nil
	a.store = nil
}

func (a *app) recordUsage(r usageRecord) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.store == nil {
		return fmt.Errorf("plugin is not configured")
	}
	if !a.cfg.Enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, ok, err := a.store.getPrice(ctx, r.Model)
	if err != nil {
		return err
	}
	cost := float64(0)
	if ok {
		cost = calculateCost(p, r.Detail, r.ServiceTier, a.cfg)
	}
	requested := r.RequestedAt.Unix()
	if r.RequestedAt.IsZero() {
		requested = time.Now().Unix()
	}
	observed := requested
	if !r.RequestedAt.IsZero() {
		delay := time.Duration(r.TTFT)
		if delay <= 0 {
			delay = time.Duration(r.Latency)
		}
		if delay > 0 && delay <= 24*time.Hour {
			observed = r.RequestedAt.Add(delay).Unix()
		}
	}
	total := r.Detail.TotalTokens
	if total == 0 {
		total = r.Detail.InputTokens + r.Detail.OutputTokens
	}
	used, hasUsed := headerFloat(r.ResponseHeaders, "X-Codex-Primary-Used-Percent")
	reset, _ := headerInt(r.ResponseHeaders, "X-Codex-Primary-Reset-At")
	// reset_at is occasionally off by a few seconds between concurrent
	// responses because some upstreams derive it from reset_after_seconds.
	// Canonicalize to a minute so one quota cycle is not split into many.
	reset = canonicalResetAt(reset)
	window, _ := headerInt(r.ResponseHeaders, "X-Codex-Primary-Window-Minutes")
	var usedPtr *float64
	if hasUsed {
		usedPtr = &used
	}

	// Plus accounts can expose a 5-hour primary window and an independent
	// weekly secondary window. Persist the secondary quota only when the
	// primary header actually identifies a 5-hour window, so accounts with a
	// single weekly quota keep the existing one-window behavior.
	secondaryUsed, hasSecondaryUsed := headerFloat(r.ResponseHeaders, "X-Codex-Secondary-Used-Percent")
	secondaryReset, hasSecondaryReset := headerInt(r.ResponseHeaders, "X-Codex-Secondary-Reset-At")
	secondaryWindow, hasSecondaryWindow := headerInt(r.ResponseHeaders, "X-Codex-Secondary-Window-Minutes")
	var secondaryUsedPtr *float64
	if isFiveHourWindow(window) && hasSecondaryUsed && hasSecondaryReset && hasSecondaryWindow && secondaryReset > 0 && isWeeklyWindow(secondaryWindow) {
		secondaryUsedPtr = &secondaryUsed
		secondaryReset = canonicalResetAt(secondaryReset)
	} else {
		secondaryReset = 0
		secondaryWindow = 0
	}
	account := strings.TrimSpace(r.AuthID)
	if account == "" {
		account = strings.TrimSpace(r.AuthIndex)
	}
	if account == "" {
		account = "unknown"
	}
	e := event{RequestedAt: requested, ObservedAt: observed, Account: account, Provider: r.Provider, Model: r.Model, Alias: r.Alias, ServiceTier: r.ServiceTier, InputTokens: r.Detail.InputTokens, OutputTokens: r.Detail.OutputTokens, ReasoningTokens: r.Detail.ReasoningTokens, CacheReadTokens: max(r.Detail.CacheReadTokens, r.Detail.CachedTokens), CacheWriteTokens: r.Detail.CacheCreationTokens, TotalTokens: total, CostUSD: cost, Failed: r.Failed, StatusCode: r.Failure.StatusCode, UsedPercent: usedPtr, ResetAt: reset, WindowMinutes: window, SecondaryUsedPercent: secondaryUsedPtr, SecondaryResetAt: secondaryReset, SecondaryWindowMinutes: secondaryWindow, PlanType: header(r.ResponseHeaders, "X-Codex-Plan-Type"), QuotaScope: quotaScopeForUsage(r.Model, r.Alias)}
	return a.store.insertEvent(ctx, e, time.Duration(a.cfg.SampleIntervalMinutes)*time.Minute)
}

func canonicalResetAt(resetAt int64) int64 {
	if resetAt <= 0 {
		return 0
	}
	return ((resetAt + 30) / 60) * 60
}

func header(h map[string][]string, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
	}
	return ""
}
func headerFloat(h map[string][]string, key string) (float64, bool) {
	v, err := strconv.ParseFloat(header(h, key), 64)
	return v, err == nil
}
func headerInt(h map[string][]string, key string) (int64, bool) {
	v, err := strconv.ParseInt(header(h, key), 10, 64)
	return v, err == nil
}

func jsonResponse(status int, v any) managementResponse {
	body, err := json.Marshal(v)
	if err != nil {
		return textResponse(500, err.Error())
	}
	return managementResponse{StatusCode: status, Headers: map[string][]string{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: body}
}
func textResponse(status int, text string) managementResponse {
	return managementResponse{StatusCode: status, Headers: map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}}, Body: []byte(text)}
}

var _ = sql.ErrNoRows
