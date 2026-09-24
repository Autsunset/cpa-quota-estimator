package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type app struct {
	mu                          sync.RWMutex
	cfg                         config
	store                       *store
	cancel                      context.CancelFunc
	pricingTaskMu               sync.Mutex
	pricingTask                 *pricingRecalcTask
	pricingTaskHistory          map[string]pricingRecalcTask
	pricingProgressHook         func(pricingRecalcProgress) error
	attributionWriter           func(context.Context, *store, *weightFit, int64) (int64, error)
	insertEventWriter           func(context.Context, event, time.Duration) error
	usageRetryBudget            time.Duration
	droppedUsagePending         atomic.Int64
	dropFlushRunning            atomic.Bool
	pricingRepricePending       bool
	pricingRepriceIncludeCustom bool
	pricingFitPending           bool
	lastPricedFit               *weightFit
	lastAutoFitRepriceAt        int64
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
				{"Name": "pricing_mode", "Type": "enum", "EnumValues": []string{pricingModeAPI, pricingModeCredits, pricingModeCustom}, "Description": "计价口径：官方 API 美元价格、Codex Credits 或自定义美元价格"},
				{"Name": "anchor_model", "Type": "string", "Description": "API/Credits 计价锚定模型，默认 gpt-5.6-sol"},
				{"Name": "capture_codex_headers", "Type": "boolean", "Description": "原样记录 X-Codex-* 响应头，默认关闭"},
				{"Name": "weight_half_life_days", "Type": "number", "Description": "学习器时间衰减半衰期，默认 21 天"},
				{"Name": "weight_random_walk_sigma", "Type": "number", "Description": "周期间 log 尺度随机游走标准差，默认 0.35"},
				{"Name": "weight_fit_interval_minutes", "Type": "integer", "Description": "后台重拟合间隔，默认 60 分钟"},
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
			{"Method": "GET", "Path": base + "/usage"},
			{"Method": "GET", "Path": base + "/weights"},
			{"Method": "GET", "Path": base + "/weights/backtest"},
			{"Method": "GET", "Path": base + "/calibration"},
			{"Method": "GET", "Path": base + "/calibration/options"},
			{"Method": "POST", "Path": base + "/calibration/start"},
			{"Method": "POST", "Path": base + "/calibration/end"},
			{"Method": "POST", "Path": base + "/calibration/cancel"},
			{"Method": "GET", "Path": base + "/summary"},
			{"Method": "GET", "Path": base + "/series"},
			{"Method": "GET", "Path": base + "/monthly"},
			{"Method": "GET", "Path": base + "/repair/early-resets"},
			{"Method": "POST", "Path": base + "/repair/early-resets"},
			{"Method": "GET", "Path": base + "/prices"},
			{"Method": "POST", "Path": base + "/prices/sync"},
			{"Method": "GET", "Path": base + "/pricing-settings"},
			{"Method": "GET", "Path": base + "/pricing-settings/task"},
			{"Method": "POST", "Path": base + "/pricing-settings"},
			{"Method": "GET", "Path": base + "/coverage-settings"},
			{"Method": "POST", "Path": base + "/coverage-settings"},
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
	rawMode := strings.ToLower(strings.TrimSpace(cfg.PricingMode))
	cfg.PricingMode = normalizePricingMode(cfg.PricingMode)
	cfg.PricingMigration = rawMode != "" && rawMode != cfg.PricingMode
	if cfg.AnchorModel == "" {
		cfg.AnchorModel = "gpt-5.6-sol"
	}
	if cfg.LongContextThreshold <= 0 {
		cfg.LongContextThreshold = 272000
	}
	if cfg.HistoryDays < 7 {
		cfg.HistoryDays = 365
	}
	if cfg.WeightHalfLifeDays <= 0 {
		cfg.WeightHalfLifeDays = 21
	}
	if cfg.WeightRandomWalkSigma <= 0 {
		cfg.WeightRandomWalkSigma = .35
	}
	if cfg.WeightFitIntervalMinutes < 5 {
		cfg.WeightFitIntervalMinutes = 60
	}
	return cfg, nil
}

func fitNeedsRefresh(result weightBacktest, hasFit bool, cfg config) bool {
	if !hasFit {
		return true
	}
	if result.FittedWeights.RandomWalkSigma != cfg.WeightRandomWalkSigma || result.FittedWeights.HalfLifeDays != cfg.WeightHalfLifeDays {
		return true
	}
	return time.Now().Unix()-result.GeneratedAt > int64(cfg.WeightFitIntervalMinutes*60)
}

func (a *app) configure(raw []byte) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	if task, ok := a.latestPricingTask(""); ok && (task.Status == "queued" || task.Status == "running" || task.Status == "rolling_back") {
		return fmt.Errorf("pricing recalculation is in progress")
	}
	a.mu.RLock()
	existing := a.store
	samePath := existing != nil && a.cfg.DataPath == cfg.DataPath
	a.mu.RUnlock()
	s := existing
	opened := false
	if !samePath {
		s, err = openStore(cfg.DataPath)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		opened = true
		if err = seedPrices(context.Background(), s); err != nil {
			s.close()
			return err
		}
	}
	catalog, err := loadPriceCatalog(context.Background(), s)
	if err != nil {
		if opened {
			s.close()
		}
		return err
	}
	cfg.PriceCatalog = catalog
	settings, err := s.loadPricingSettings(context.Background(), cfg.pricingSettings())
	if err != nil {
		if opened {
			s.close()
		}
		return err
	}
	cfg = cfg.withPricingSettings(settings)
	fitResult, hasFit, err := s.latestWeightFit(context.Background())
	if err != nil {
		if opened {
			s.close()
		}
		return err
	}
	if hasFit && fitResult.FittedWeights.Available {
		cfg.LearnedFit = &fitResult.FittedWeights
		_ = s.seedOnlineCycleScales(context.Background(), cfg.LearnedFit)
	}
	lastAutoFit, err := s.lastFitRepriceAt(context.Background())
	if err != nil {
		if opened {
			s.close()
		}
		return err
	}
	a.mu.Lock()
	if a.store != existing {
		a.mu.Unlock()
		if opened {
			s.close()
		}
		return fmt.Errorf("store changed while configuring")
	}
	oldStore, oldCancel := a.store, a.cancel
	a.store, a.cfg = s, cfg
	a.pricingTaskMu.Lock()
	a.lastAutoFitRepriceAt = lastAutoFit
	a.lastPricedFit = cfg.LearnedFit
	if settings.Migrated {
		a.lastPricedFit = nil
	}
	a.pricingTaskMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.mu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	if oldStore != nil && oldStore != s {
		_ = oldStore.close()
	}
	if settings.Migrated {
		if _, taskErr := a.startPricingTask(s, settings, cfg); taskErr != nil {
			return fmt.Errorf("schedule pricing migration: %w", taskErr)
		}
	}
	go a.background(ctx, s, cfg)
	return nil
}

func loadPriceCatalog(ctx context.Context, s *store) (map[string]price, error) {
	prices, err := s.listPrices(ctx)
	if err != nil {
		return nil, err
	}
	byModel := make(map[string]price, len(prices))
	for _, p := range prices {
		byModel[normalizeModel(p.Model)] = p
	}
	return byModel, nil
}

func (a *app) background(ctx context.Context, s *store, cfg config) {
	a.refreshWeightsIfNeeded(ctx, s, cfg)
	if _, err := syncPrices(ctx, s, cfg); err == nil {
		a.refreshPriceCatalog(ctx, s)
	}
	_, _ = s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('last_price_sync_attempt',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatInt(time.Now().Unix(), 10))
	ticker := time.NewTicker(time.Duration(cfg.PriceSyncIntervalMinutes) * time.Minute)
	defer ticker.Stop()
	cleanup := time.NewTicker(24 * time.Hour)
	defer cleanup.Stop()
	weights := time.NewTicker(time.Duration(cfg.WeightFitIntervalMinutes) * time.Minute)
	defer weights.Stop()
	drops := time.NewTicker(30 * time.Second)
	defer drops.Stop()
	checkpoint := time.NewTicker(30 * time.Second)
	defer checkpoint.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := syncPrices(ctx, s, cfg); err == nil {
				a.refreshPriceCatalog(ctx, s)
			}
		case <-cleanup.C:
			_ = s.cleanup(ctx, cfg.HistoryDays)
			_ = s.rebuildAllQuotaSegments(ctx)
		case <-weights.C:
			a.refreshWeightsIfNeeded(ctx, s, cfg)
		case <-drops.C:
			a.startDropFlush(s)
		case <-checkpoint.C:
			_ = s.checkpointWAL(ctx)
		}
	}
}

func (a *app) refreshWeightsIfNeeded(ctx context.Context, s *store, cfg config) {
	var newestSegment int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(end_at),0) FROM quota_segments`).Scan(&newestSegment); err != nil {
		return
	}
	previous, hasPrevious, err := s.latestWeightFit(ctx)
	if err != nil {
		return
	}
	if hasPrevious && newestSegment <= previous.GeneratedAt && !fitNeedsRefresh(previous, hasPrevious, cfg) && time.Now().Unix()-previous.GeneratedAt < 24*3600 {
		return
	}
	fitResult, err := s.refreshWeightFit(ctx, weightLearnerOptions{HalfLifeDays: cfg.WeightHalfLifeDays, MaxIterations: 80, RandomWalkSigma: cfg.WeightRandomWalkSigma})
	if err != nil || !fitResult.FittedWeights.Available || ctx.Err() != nil {
		return
	}
	a.applyFittedWeights(ctx, s, &fitResult.FittedWeights)
}

func (a *app) applyFittedWeights(ctx context.Context, s *store, fit *weightFit) {
	a.mu.Lock()
	if a.store != s || ctx.Err() != nil {
		a.mu.Unlock()
		return
	}
	a.cfg.LearnedFit = fit
	cfg := a.cfg
	a.mu.Unlock()
	_ = s.seedOnlineCycleScales(ctx, fit)
	writer := a.attributionWriter
	if writer == nil {
		writer = func(ctx context.Context, s *store, fit *weightFit, threshold int64) (int64, error) {
			return s.updateWeightAttributions(ctx, fit, threshold)
		}
	}
	_, _ = writer(ctx, s, fit, cfg.LongContextThreshold)
	a.queueFitRepricing(s, cfg)
}

func (a *app) refreshPriceCatalog(ctx context.Context, s *store) {
	catalog, err := loadPriceCatalog(ctx, s)
	if err != nil {
		return
	}
	var changed bool
	var current config
	a.mu.Lock()
	if a.store == s {
		changed = !samePriceCatalogRates(a.cfg.PriceCatalog, catalog)
		a.cfg.PriceCatalog = catalog
		current = a.cfg
	}
	a.mu.Unlock()
	if changed {
		a.queuePricingReprice(s, current, true)
	}
}

func samePriceCatalogRates(left, right map[string]price) bool {
	if len(left) != len(right) {
		return false
	}
	for model, a := range left {
		b, ok := right[model]
		if !ok || a.Input != b.Input || a.Output != b.Output || a.CacheRead != b.CacheRead || a.CacheWrite != b.CacheWrite || a.LongInput != b.LongInput || a.LongOutput != b.LongOutput || a.LongRead != b.LongRead || a.LongWrite != b.LongWrite || a.FastInput != b.FastInput || a.FastOutput != b.FastOutput || a.FastRead != b.FastRead || a.FastWrite != b.FastWrite {
			return false
		}
	}
	return true
}

func (a *app) shutdown() {
	a.mu.Lock()
	cancel, s := a.cancel, a.store
	a.cancel = nil
	a.store = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s != nil {
		if pending := a.droppedUsagePending.Swap(0); pending > 0 {
			ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.addDroppedUsageCount(ctx, pending); err != nil {
				a.droppedUsagePending.Add(pending)
			}
			done()
		}
		_ = s.close()
	}
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
	e.IngestID = newUsageIngestID()
	if a.cfg.LearnedFit != nil && !r.Failed {
		e.LearnedFit = a.cfg.LearnedFit
		e.LearnedQuotaPct = a.store.learnedQuotaAttributionLive(ctx, a.cfg.LearnedFit, account, e.QuotaScope, r.Model, r.ServiceTier, e.ResetAt, r.Detail, a.cfg.LongContextThreshold)
		if secondaryUsedPtr != nil {
			secondaryScope := weeklyQuotaScope
			if e.QuotaScope == sparkQuotaScope {
				secondaryScope = sparkWeeklyQuotaScope
			}
			e.LearnedSecondaryPct = a.store.learnedQuotaAttributionLive(ctx, a.cfg.LearnedFit, account, secondaryScope, r.Model, r.ServiceTier, e.SecondaryResetAt, r.Detail, a.cfg.LongContextThreshold)
		}
	}
	if a.cfg.CaptureCodexHeaders {
		captured := make(map[string][]string)
		for name, values := range r.ResponseHeaders {
			if strings.HasPrefix(strings.ToLower(name), "x-codex-") && !strings.EqualFold(name, "X-Codex-Turn-State") {
				captured[name] = append([]string(nil), values...)
			}
		}
		if len(captured) > 0 {
			raw, err := json.Marshal(captured)
			if err != nil {
				return err
			}
			e.CodexHeadersJSON = string(raw)
		}
	}
	if err := a.insertUsageWithRetry(a.store, e, time.Duration(a.cfg.SampleIntervalMinutes)*time.Minute); err != nil {
		return err
	}
	claimed, err := a.store.claimCalibrationRefit(ctx, account)
	if err != nil {
		return err
	}
	if claimed {
		go a.runGuidedCalibrationRefit(a.store, a.cfg, account)
	}
	return nil
}

func (a *app) runGuidedCalibrationRefit(s *store, cfg config, account string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := s.refreshWeightFit(ctx, weightLearnerOptions{HalfLifeDays: cfg.WeightHalfLifeDays, MaxIterations: 80, RandomWalkSigma: cfg.WeightRandomWalkSigma})
	if err == nil && !result.FittedWeights.Available {
		err = fmt.Errorf("weight fit unavailable")
	}
	var after *weightEstimate
	if err == nil {
		session, ok, readErr := s.calibrationSession(ctx, account)
		if readErr != nil {
			err = readErr
		} else if ok {
			after = calibrationWeight(&result.FittedWeights, session.ModelB)
		}
	}
	if err == nil {
		a.applyFittedWeights(ctx, s, &result.FittedWeights)
	}
	_ = s.finishCalibrationRefit(context.Background(), account, after, err)
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
