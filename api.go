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
	case strings.HasSuffix(req.Path, "/overview"):
		accounts, err := a.store.accounts(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		items := make([]accountOverview, 0, len(accounts))
		now := time.Now().Unix()
		for _, account := range accounts {
			item, errOverview := a.buildAccountOverview(ctx, account, now)
			if errOverview != nil {
				return textResponse(500, errOverview.Error())
			}
			items = append(items, item)
		}
		return jsonResponse(200, overviewResponse{
			PluginVersion: pluginVersion,
			PricingMode:   normalizePricingMode(a.cfg.PricingMode),
			ValueUnit:     pricingValueUnit(a.cfg.PricingMode),
			Accounts:      items,
		})
	case strings.HasSuffix(req.Path, "/summary"):
		account := req.Query.Get("account")
		accounts, err := a.store.accounts(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		if account == "" && len(accounts) > 0 {
			account = accounts[0]
		}
		resp := map[string]any{"plugin_version": pluginVersion, "account": account, "accounts": accounts, "config": map[string]any{"model_price_multipliers": modelPriceMultipliers(), "fast_pricing_mode": a.cfg.FastPricingMode, "fast_multiplier": a.cfg.FastMultiplier, "pricing_mode": normalizePricingMode(a.cfg.PricingMode), "value_unit": pricingValueUnit(a.cfg.PricingMode), "apply_fast_pricing": a.cfg.ApplyFastPricing, "long_context_threshold": a.cfg.LongContextThreshold, "apply_long_context_pricing": a.cfg.ApplyLongContextPricing, "price_source_url": a.cfg.PriceSourceURL}}
		if account != "" {
			hasWeeklyQuota, errWeekly := a.store.hasFiveHourWeeklyQuota(ctx, account)
			if errWeekly != nil {
				return textResponse(500, errWeekly.Error())
			}
			resp["five_hour_quota_detected"] = hasWeeklyQuota
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
			estimate := estimateCapacity(points)
			resp["estimate"] = estimate
			allowances, errAllowance := a.store.remainingModelAllowances(ctx, estimate.RemainingCostUSD, a.cfg)
			if errAllowance != nil {
				return textResponse(500, errAllowance.Error())
			}
			resp["remaining_by_model"] = allowances
			resp["burn_forecast"] = estimateBurn(points, forecastReference(points, isCurrent))
			if len(points) > 0 {
				latest := points[len(points)-1]
				resp["latest"] = latest
				resp["remaining_percent"], resp["quota_status"] = quotaSnapshotState(latest, time.Now().Unix())
				resp["window_start"] = selected.StartedAt
			} else if selected.ScheduleInferred {
				resp["quota_status"] = "awaiting_refresh"
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
		estimate := estimateCapacity(points)
		allowances, errAllowance := a.store.remainingModelAllowances(ctx, estimate.RemainingCostUSD, a.cfg)
		if errAllowance != nil {
			return textResponse(500, errAllowance.Error())
		}
		response := map[string]any{"account": account, "plan_type": plan, "selected_cycle_id": selected.ID, "selected_reset_at": selected.ResetAt, "is_current": isCurrent, "cycle": selected, "points": points, "capacity_points": capacityHistory(points), "range_points": rangePoints, "range_capacity_points": capacityHistoryForCycles(rangePoints), "range_cycles": rangeCycles, "estimate": estimate, "remaining_by_model": allowances, "pricing_mode": normalizePricingMode(a.cfg.PricingMode), "value_unit": pricingValueUnit(a.cfg.PricingMode), "burn_forecast": estimateBurn(points, forecastReference(points, isCurrent))}
		hasWeeklyQuota, errWeekly := a.store.hasFiveHourWeeklyQuota(ctx, account)
		if errWeekly != nil {
			return textResponse(500, errWeekly.Error())
		}
		response["five_hour_quota_detected"] = hasWeeklyQuota
		if hasWeeklyQuota {
			weeklySeries, errQuota := a.store.latestQuotaScopeSeries(ctx, account, weeklyQuotaScope, limit)
			if errQuota != nil {
				return textResponse(500, errQuota.Error())
			}
			weeklySeries.RemainingByModel, errQuota = a.store.remainingModelAllowances(ctx, weeklySeries.Estimate.RemainingCostUSD, a.cfg)
			if errQuota != nil {
				return textResponse(500, errQuota.Error())
			}
			response["weekly_quota"] = weeklySeries
		}
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
		response := map[string]any{"account": account, "months": months, "summary": monthly, "pricing_mode": normalizePricingMode(a.cfg.PricingMode), "value_unit": pricingValueUnit(a.cfg.PricingMode)}
		hasWeeklyQuota, errWeekly := a.store.hasFiveHourWeeklyQuota(ctx, account)
		if errWeekly != nil {
			return textResponse(500, errWeekly.Error())
		}
		response["five_hour_quota_detected"] = hasWeeklyQuota
		if hasWeeklyQuota {
			weeklyMonthly, errQuota := a.store.monthlyQuotaScope(ctx, account, weeklyQuotaScope, selectedMonth)
			if errQuota != nil {
				return textResponse(500, errQuota.Error())
			}
			response["weekly_summary"] = weeklyMonthly
		}
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
		mode := strings.TrimSpace(update.PricingMode)
		if mode == "" {
			mode = a.cfg.PricingMode
		}
		if !validPricingMode(mode) {
			return textResponse(400, "pricing_mode must be current_api, legacy_api, or credits")
		}
		settings := pricingSettings{ApplyLongContext: *update.ApplyLongContext, ApplyFast: *update.ApplyFast, PricingMode: normalizePricingMode(mode)}
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
		return jsonResponse(200, map[string]any{"prices": prices, "model_price_multipliers": modelPriceMultipliers(), "fast_pricing_mode": a.cfg.FastPricingMode, "fast_multiplier": a.cfg.FastMultiplier, "pricing_mode": normalizePricingMode(a.cfg.PricingMode), "value_unit": pricingValueUnit(a.cfg.PricingMode), "apply_fast_pricing": a.cfg.ApplyFastPricing, "long_context_threshold": a.cfg.LongContextThreshold, "apply_long_context_pricing": a.cfg.ApplyLongContextPricing})
	default:
		return textResponse(404, "not found")
	}
}

func (a *app) buildAccountOverview(ctx context.Context, account string, now int64) (accountOverview, error) {
	item := accountOverview{Account: account}
	cycles, err := a.store.cycles(ctx, account, 1)
	if err != nil || len(cycles) == 0 {
		return item, err
	}

	selected := cycles[0]
	points, plan, err := a.store.pointsForCycle(ctx, account, selected.ID, 5000)
	if err != nil && err != sql.ErrNoRows {
		return item, err
	}
	item.PlanType = plan
	item.SelectedCycleID = selected.ID
	item.WindowStart = selected.StartedAt
	item.ResetAt = selected.ResetAt
	item.WindowMinutes = selected.WindowMinutes
	item.IsCurrent = selected.Current
	item.ScheduleInferred = selected.ScheduleInferred
	item.Estimate = estimateCapacity(points)
	item.BurnForecast = estimateBurn(points, forecastReference(points, selected.Current))
	if len(points) > 0 {
		latest := points[len(points)-1]
		item.RemainingPercent, item.QuotaStatus = quotaSnapshotState(latest, now)
		item.Latest = &latest
	} else if selected.ScheduleInferred || selected.ResetAt > 0 && now >= selected.ResetAt {
		item.QuotaStatus = "awaiting_refresh"
	}

	hasWeeklyQuota, err := a.store.hasFiveHourWeeklyQuota(ctx, account)
	if err != nil {
		return item, err
	}
	item.FiveHourQuotaDetected = hasWeeklyQuota
	if !hasWeeklyQuota {
		return item, nil
	}

	weekly, err := a.store.latestQuotaScopeSeriesAt(ctx, account, weeklyQuotaScope, 5000, now)
	if err != nil {
		return item, err
	}
	item.WeeklyQuota = weeklyOverview(weekly, now)
	if item.PlanType == "" {
		item.PlanType = weekly.PlanType
	}
	return item, nil
}

func weeklyOverview(series scopedQuotaSeries, now int64) *accountQuotaOverview {
	overview := &accountQuotaOverview{
		Scope:            weeklyQuotaScope,
		WindowStart:      series.StartedAt,
		ResetAt:          series.ResetAt,
		WindowMinutes:    series.WindowMinutes,
		ScheduleInferred: series.ScheduleInferred,
		Estimate:         series.Estimate,
	}
	points := make([]quotaPoint, 0, len(series.Points))
	for _, point := range series.Points {
		points = append(points, quotaPoint{
			CycleStart:    series.StartedAt,
			Time:          point.Time,
			UsedPercent:   point.UsedPercent,
			ResetAt:       point.ResetAt,
			WindowMinutes: point.WindowMinutes,
			WindowTokens:  point.WindowTokens,
			WindowCostUSD: point.WindowCostUSD,
			Requests:      point.Requests,
		})
	}
	overview.BurnForecast = estimateBurn(points, now)
	if len(series.Points) > 0 {
		latest := series.Points[len(series.Points)-1]
		overview.RemainingPercent, overview.QuotaStatus = quotaState(latest.UsedPercent, latest.ResetAt, now)
		overview.Latest = &latest
	} else if series.ScheduleInferred || series.ResetAt > 0 && now >= series.ResetAt {
		overview.QuotaStatus = "awaiting_refresh"
	}
	return overview
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

func quotaSnapshotState(point quotaPoint, now int64) (float64, string) {
	return quotaState(point.UsedPercent, point.ResetAt, now)
}

func quotaState(usedPercent float64, resetAt, now int64) (float64, string) {
	used := max(float64(0), min(float64(100), usedPercent))
	remaining := 100 - used
	if remaining <= 0.5 {
		return remaining, "exhausted"
	}
	if resetAt > 0 && now >= resetAt {
		return remaining, "awaiting_refresh"
	}
	return remaining, "active"
}

func pricingSettingsResponse(cfg config, recalculated int64) map[string]any {
	return map[string]any{
		"apply_long_context_pricing": cfg.ApplyLongContextPricing,
		"apply_fast_pricing":         cfg.ApplyFastPricing,
		"pricing_mode":               normalizePricingMode(cfg.PricingMode),
		"value_unit":                 pricingValueUnit(cfg.PricingMode),
		"long_context_threshold":     cfg.LongContextThreshold,
		"fast_pricing_mode":          cfg.FastPricingMode,
		"fast_multiplier":            cfg.FastMultiplier,
		"recalculated_events":        recalculated,
		"model_price_multipliers":    modelPriceMultipliers(),
	}
}
