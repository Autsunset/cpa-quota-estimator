package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

//go:embed web/dashboard.html
var dashboardHTML []byte

func (a *app) handleManagement(req managementRequest) managementResponse {
	if strings.HasSuffix(req.Path, "/dashboard") {
		return managementResponse{StatusCode: 200, Headers: map[string][]string{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: dashboardHTML}
	}
	if (strings.HasSuffix(req.Path, "/repair/early-resets") || strings.HasSuffix(req.Path, "/pricing-settings")) && strings.EqualFold(req.Method, "POST") {
		a.mu.Lock()
		defer a.mu.Unlock()
	} else {
		a.mu.RLock()
		defer a.mu.RUnlock()
	}
	if a.store == nil {
		return textResponse(503, "plugin store is not ready")
	}
	timeout := 30 * time.Second
	if strings.HasSuffix(req.Path, "/pricing-settings") && strings.EqualFold(req.Method, "POST") {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	switch {
	case strings.HasSuffix(req.Path, "/summary"):
		account := req.Query.Get("account")
		accounts, err := a.store.accounts(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		if account == "" && len(accounts) > 0 {
			account = accounts[0]
		}
		resp := map[string]any{"plugin_version": pluginVersion, "account": account, "accounts": accounts, "config": map[string]any{"fast_pricing_mode": a.cfg.FastPricingMode, "fast_multiplier": a.cfg.FastMultiplier, "apply_fast_pricing": a.cfg.ApplyFastPricing, "long_context_threshold": a.cfg.LongContextThreshold, "apply_long_context_pricing": a.cfg.ApplyLongContextPricing, "price_source_url": a.cfg.PriceSourceURL}}
		if account != "" {
			cycles, err := a.store.cycles(ctx, account, 60)
			if err != nil {
				return textResponse(500, err.Error())
			}
			windows, err := a.store.windows(ctx, account, 60)
			if err != nil {
				return textResponse(500, err.Error())
			}
			selected, isCurrent := selectCycle(cycles, req.Query.Get("cycle_id"), req.Query.Get("reset_at"))
			resp["cycles"] = cycles
			resp["windows"] = windows
			resp["selected_cycle_id"] = selected.ID
			resp["selected_reset_at"] = selected.ResetAt
			resp["is_current"] = isCurrent
			points, plan, err := a.store.pointsForCycle(ctx, account, selected.ID, 5000)
			if err != nil && err != sql.ErrNoRows {
				return textResponse(500, err.Error())
			}
			resp["plan_type"] = plan
			resp["estimate"] = estimateCapacity(points)
			resp["burn_forecast"] = estimateBurn(points, forecastReference(points, isCurrent))
			if len(points) > 0 {
				resp["latest"] = points[len(points)-1]
				resp["window_start"] = selected.StartedAt
			}
		}
		return jsonResponse(200, resp)
	case strings.HasSuffix(req.Path, "/series"):
		account := req.Query.Get("account")
		if account == "" {
			accounts, _ := a.store.accounts(ctx)
			if len(accounts) > 0 {
				account = accounts[0]
			}
		}
		limit, _ := strconv.Atoi(req.Query.Get("limit"))
		cycles, err := a.store.cycles(ctx, account, 60)
		if err != nil {
			return textResponse(500, err.Error())
		}
		selected, isCurrent := selectCycle(cycles, req.Query.Get("cycle_id"), req.Query.Get("reset_at"))
		points, plan, err := a.store.pointsForCycle(ctx, account, selected.ID, limit)
		if err != nil && err != sql.ErrNoRows {
			return textResponse(500, err.Error())
		}
		rangePoints := points
		rangeCycles := []quotaCycle{selected}
		startAt, _ := strconv.ParseInt(req.Query.Get("start_at"), 10, 64)
		endAt, _ := strconv.ParseInt(req.Query.Get("end_at"), 10, 64)
		if startAt > 0 || endAt > 0 {
			if startAt <= 0 || endAt <= startAt {
				return textResponse(400, "invalid chart range")
			}
			rangePoints, rangeCycles, err = a.store.pointsForRange(ctx, account, startAt, endAt, limit)
			if err != nil {
				return textResponse(500, err.Error())
			}
		}
		response := map[string]any{"account": account, "plan_type": plan, "selected_cycle_id": selected.ID, "selected_reset_at": selected.ResetAt, "is_current": isCurrent, "cycle": selected, "points": points, "capacity_points": capacityHistory(points), "range_points": rangePoints, "range_capacity_points": capacityHistoryForCycles(rangePoints), "range_cycles": rangeCycles, "estimate": estimateCapacity(points), "burn_forecast": estimateBurn(points, forecastReference(points, isCurrent))}
		if req.Query.Get("include_spark") == "1" {
			sparkSeries, errSpark := a.store.latestQuotaScopeSeries(ctx, account, sparkQuotaScope, limit)
			if errSpark != nil {
				return textResponse(500, errSpark.Error())
			}
			response["spark_quota"] = sparkSeries
		}
		return jsonResponse(200, response)
	case strings.HasSuffix(req.Path, "/monthly"):
		account := req.Query.Get("account")
		if account == "" {
			accounts, _ := a.store.accounts(ctx)
			if len(accounts) > 0 {
				account = accounts[0]
			}
		}
		months, err := a.store.months(ctx, account, 36)
		if err != nil {
			return textResponse(500, err.Error())
		}
		selectedMonth := req.Query.Get("month")
		if selectedMonth == "" && len(months) > 0 {
			selectedMonth = months[0]
		}
		if _, _, _, err = monthRange(selectedMonth); err != nil {
			return textResponse(400, err.Error())
		}
		monthly, err := a.store.monthly(ctx, account, selectedMonth)
		if err != nil {
			return textResponse(500, err.Error())
		}
		response := map[string]any{"account": account, "months": months, "summary": monthly}
		if req.Query.Get("include_spark") == "1" {
			sparkMonthly, errSpark := a.store.monthlyQuotaScope(ctx, account, sparkQuotaScope, selectedMonth)
			if errSpark != nil {
				return textResponse(500, errSpark.Error())
			}
			response["spark_summary"] = sparkMonthly
		}
		return jsonResponse(200, response)
	case strings.HasSuffix(req.Path, "/repair/early-resets"):
		account := req.Query.Get("account")
		if account == "" {
			return textResponse(400, "account is required")
		}
		apply := strings.EqualFold(req.Method, "POST")
		report, err := a.store.repairFalseEarlyResets(ctx, account, apply)
		if err != nil {
			return textResponse(500, err.Error())
		}
		return jsonResponse(200, report)
	case strings.HasSuffix(req.Path, "/pricing-settings"):
		if strings.EqualFold(req.Method, "GET") {
			return jsonResponse(200, pricingSettingsResponse(a.cfg, 0))
		}
		if !strings.EqualFold(req.Method, "POST") {
			return textResponse(405, "method not allowed")
		}
		var update pricingSettingsUpdate
		if err := json.Unmarshal(req.Body, &update); err != nil {
			return textResponse(400, "invalid pricing settings: "+err.Error())
		}
		if update.ApplyLongContext == nil || update.ApplyFast == nil {
			return textResponse(400, "apply_long_context_pricing and apply_fast_pricing are required")
		}
		settings := pricingSettings{ApplyLongContext: *update.ApplyLongContext, ApplyFast: *update.ApplyFast}
		cfg := a.cfg.withPricingSettings(settings)
		recalculated, err := a.store.savePricingSettingsAndRecalculate(ctx, settings, cfg)
		if err != nil {
			return textResponse(500, err.Error())
		}
		a.cfg = cfg
		return jsonResponse(200, pricingSettingsResponse(a.cfg, recalculated))
	case strings.HasSuffix(req.Path, "/prices/sync"):
		count, err := syncPrices(ctx, a.store, a.cfg)
		if err != nil {
			return textResponse(502, err.Error())
		}
		return jsonResponse(200, map[string]any{"ok": true, "count": count, "source": a.cfg.PriceSourceURL})
	case strings.HasSuffix(req.Path, "/prices"):
		prices, err := a.store.listPrices(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		return jsonResponse(200, map[string]any{"prices": prices, "fast_pricing_mode": a.cfg.FastPricingMode, "fast_multiplier": a.cfg.FastMultiplier, "apply_fast_pricing": a.cfg.ApplyFastPricing, "long_context_threshold": a.cfg.LongContextThreshold, "apply_long_context_pricing": a.cfg.ApplyLongContextPricing})
	default:
		return textResponse(404, "not found")
	}
}

func selectCycle(cycles []quotaCycle, rawCycleID, rawResetAt string) (quotaCycle, bool) {
	if len(cycles) == 0 {
		return quotaCycle{}, false
	}
	requestedID, _ := strconv.ParseInt(rawCycleID, 10, 64)
	requestedReset, _ := strconv.ParseInt(rawResetAt, 10, 64)
	for _, cycle := range cycles {
		if (requestedID > 0 && cycle.ID == requestedID) ||
			(requestedID == 0 && requestedReset > 0 && cycle.ResetAt == requestedReset) {
			return cycle, cycle.Current
		}
	}
	return cycles[0], cycles[0].Current
}

func selectWindow(windows []quotaWindow, rawResetAt string) (quotaWindow, bool) {
	if len(windows) == 0 {
		return quotaWindow{}, false
	}
	requested, _ := strconv.ParseInt(rawResetAt, 10, 64)
	for i, window := range windows {
		if requested > 0 && window.ResetAt == requested {
			return window, i == 0
		}
	}
	return windows[0], true
}

func forecastReference(points []quotaPoint, isCurrent bool) int64 {
	if isCurrent || len(points) == 0 {
		return time.Now().Unix()
	}
	return points[len(points)-1].Time
}

func pricingSettingsResponse(cfg config, recalculated int64) map[string]any {
	return map[string]any{
		"apply_long_context_pricing": cfg.ApplyLongContextPricing,
		"apply_fast_pricing":         cfg.ApplyFastPricing,
		"long_context_threshold":     cfg.LongContextThreshold,
		"fast_pricing_mode":          cfg.FastPricingMode,
		"fast_multiplier":            cfg.FastMultiplier,
		"recalculated_events":        recalculated,
	}
}
