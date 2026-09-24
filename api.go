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
	if strings.HasSuffix(req.Path, "/pricing-settings") && strings.EqualFold(req.Method, "POST") {
		return a.handlePricingSettingsSave(req)
	}
	if strings.HasSuffix(req.Path, "/repair/early-resets") && strings.EqualFold(req.Method, "POST") {
		return a.handleEarlyResetRepair(req)
	}
	if strings.HasSuffix(req.Path, "/coverage-settings") && strings.EqualFold(req.Method, "POST") {
		a.mu.Lock()
		defer a.mu.Unlock()
	} else {
		a.mu.RLock()
		defer a.mu.RUnlock()
	}
	if a.store == nil {
		return textResponse(503, "plugin store is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			coverage, errCoverage := a.store.accountCoverage(ctx, account)
			if errCoverage != nil {
				return textResponse(500, errCoverage.Error())
			}
			item.CollectionCoverage = &coverage
			item.Estimate = estimateForCoverage(item.Estimate, coverage)
			if item.WeeklyQuota != nil {
				item.WeeklyQuota.Estimate = estimateForCoverage(item.WeeklyQuota.Estimate, coverage)
			}
			items = append(items, item)
		}
		active, taskID := a.pricingTaskActive()
		return jsonResponse(200, overviewResponse{
			PluginVersion: pluginVersion,
			PricingMode:   normalizePricingMode(a.cfg.PricingMode),
			ValueUnit:     pricingValueUnit(a.cfg.PricingMode),
			Recalculating: active, PricingTaskID: taskID, DroppedUsageEvents: a.totalDroppedUsage(ctx, a.store),
			Accounts: items,
		})
	case strings.HasSuffix(req.Path, "/usage"):
		if !strings.EqualFold(req.Method, "GET") {
			return textResponse(405, "method not allowed")
		}
		account := req.Query.Get("account")
		if account == "" {
			accounts, err := a.store.accounts(ctx)
			if err != nil {
				return textResponse(500, err.Error())
			}
			if len(accounts) > 0 {
				account = accounts[0]
			}
		}
		days := 7
		if raw := req.Query.Get("days"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed != 1 && parsed != 7 && parsed != 30 {
				return textResponse(400, "days must be 1, 7, or 30")
			}
			days = parsed
		}
		endAt := time.Now().Unix() + 1
		usage, err := a.store.usageBreakdown(ctx, account, endAt-int64(days)*86400, endAt, days, a.cfg)
		if err != nil {
			return textResponse(500, err.Error())
		}
		usage.Recalculating, usage.PricingTaskID = a.pricingTaskActive()
		return jsonResponse(200, usage)
	case strings.HasSuffix(req.Path, "/weights/backtest"):
		if !strings.EqualFold(req.Method, "GET") {
			return textResponse(405, "method not allowed")
		}
		result, ok, err := a.store.latestWeightFit(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		if !ok {
			return jsonResponse(200, weightBacktest{Lags: []backtestLagResult{}, Scores: []backtestScore{}})
		}
		return jsonResponse(200, result)
	case strings.HasSuffix(req.Path, "/calibration/options"):
		if !strings.EqualFold(req.Method, "GET") {
			return textResponse(405, "method not allowed")
		}
		var models, locked, targets []string
		if a.cfg.LearnedFit != nil && a.cfg.LearnedFit.Available {
			for _, row := range a.cfg.LearnedFit.Models {
				models = append(models, row.Model)
				if row.Input.PriorLocked {
					locked = append(locked, row.Model)
				}
				if row.Input.PriorLocked || row.Model == "gpt-6-astra" {
					targets = append(targets, row.Model)
				}
			}
		}
		return jsonResponse(200, map[string]any{"models": models, "locked_models": locked, "target_models": targets})
	case strings.HasSuffix(req.Path, "/calibration/start"):
		if !strings.EqualFold(req.Method, "POST") {
			return textResponse(405, "method not allowed")
		}
		var body struct {
			Account       string  `json:"account"`
			ModelA        string  `json:"model_a"`
			ModelB        string  `json:"model_b"`
			TargetPercent float64 `json:"target_percent"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return textResponse(400, err.Error())
		}
		if a.cfg.LearnedFit == nil || !a.cfg.LearnedFit.Available {
			return textResponse(409, "weight fit required before calibration")
		}
		before := calibrationWeight(a.cfg.LearnedFit, body.ModelB)
		if before == nil || (!before.PriorLocked && normalizeModel(body.ModelB) != "gpt-6-astra") {
			return textResponse(400, "model_b must be Astra or currently prior-locked")
		}
		if calibrationWeight(a.cfg.LearnedFit, body.ModelA) == nil {
			return textResponse(400, "model_a must be in the weight fit")
		}
		session, err := a.store.startCalibration(ctx, body.Account, body.ModelA, body.ModelB, body.TargetPercent, before)
		if err != nil {
			return textResponse(400, err.Error())
		}
		return jsonResponse(200, session)
	case strings.HasSuffix(req.Path, "/calibration/end") || strings.HasSuffix(req.Path, "/calibration/cancel"):
		if !strings.EqualFold(req.Method, "POST") {
			return textResponse(405, "method not allowed")
		}
		var body struct {
			Account string `json:"account"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return textResponse(400, err.Error())
		}
		session, err := a.store.endOrCancelCalibration(ctx, body.Account, strings.HasSuffix(req.Path, "/cancel"))
		if err != nil {
			return textResponse(400, err.Error())
		}
		if session.RefitStatus == "pending" {
			claimed, claimErr := a.store.claimCalibrationRefit(ctx, body.Account)
			if claimErr != nil {
				return textResponse(500, claimErr.Error())
			}
			if claimed {
				go a.runGuidedCalibrationRefit(a.store, a.cfg, body.Account)
			}
		}
		return jsonResponse(200, session)
	case strings.HasSuffix(req.Path, "/calibration"):
		if !strings.EqualFold(req.Method, "GET") {
			return textResponse(405, "method not allowed")
		}
		account := req.Query.Get("account")
		if account == "" {
			return textResponse(400, "account is required")
		}
		session, ok, err := a.store.calibrationSession(ctx, account)
		if err != nil {
			return textResponse(500, err.Error())
		}
		if !ok {
			return jsonResponse(200, map[string]any{"available": false})
		}
		return jsonResponse(200, session)
	case strings.HasSuffix(req.Path, "/weights"):
		if !strings.EqualFold(req.Method, "GET") {
			return textResponse(405, "method not allowed")
		}
		result, ok, err := a.store.latestWeightFit(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		if !ok {
			return jsonResponse(200, weightFit{Models: []learnedModelWeights{}})
		}
		current, err := a.store.overlayOnlineCycleScales(ctx, result.FittedWeights)
		if err != nil {
			return textResponse(500, err.Error())
		}
		return jsonResponse(200, current)
	case strings.HasSuffix(req.Path, "/summary"):
		account := req.Query.Get("account")
		accounts, err := a.store.accounts(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		if account == "" && len(accounts) > 0 {
			account = accounts[0]
		}
		active, taskID := a.pricingTaskActive()
		resp := map[string]any{"plugin_version": pluginVersion, "account": account, "accounts": accounts, "config": pricingSettingsResponse(a.cfg), "recalculating": active, "pricing_task_id": taskID, "dropped_usage_events": a.totalDroppedUsage(ctx, a.store)}
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
			anomalies, err := a.store.quotaRegimeAnomalies(ctx, selected.ID)
			if err != nil {
				return textResponse(500, err.Error())
			}
			resp["quota_anomalies"] = anomalies
			points, plan, err := a.store.pointsForCycle(ctx, account, selected.ID, 5000)
			if err != nil && err != sql.ErrNoRows {
				return textResponse(500, err.Error())
			}
			resp["plan_type"] = plan
			estimate, err := a.store.onlineCycleCapacity(ctx, account, selected, points, a.cfg, forecastReference(points, isCurrent))
			if err != nil {
				return textResponse(500, err.Error())
			}
			resp["estimate"] = estimate
			allowances, errAllowance := a.store.remainingModelAllowances(ctx, estimate.RemainingCostUSD, a.cfg)
			if errAllowance != nil {
				return textResponse(500, errAllowance.Error())
			}
			resp["remaining_by_model"] = allowances
			resp["burn_forecast"] = burnWithOnlineCapacity(points, forecastReference(points, isCurrent), estimate)
			if len(points) > 0 {
				latest := points[len(points)-1]
				resp["latest"] = latest
				resp["remaining_percent"], resp["quota_status"] = quotaSnapshotState(latest, time.Now().Unix())
				resp["window_start"] = selected.StartedAt
			} else if selected.ScheduleInferred {
				resp["quota_status"] = "awaiting_refresh"
			}
		}
		return a.coverageResponse(ctx, account, resp)
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
		anomalies, err := a.store.quotaRegimeAnomalies(ctx, selected.ID)
		if err != nil {
			return textResponse(500, err.Error())
		}
		rangeAnomalies := anomalies
		if startAt > 0 || endAt > 0 {
			rangeAnomalies, err = a.store.quotaRegimeAnomaliesForCycles(ctx, rangeCycles)
			if err != nil {
				return textResponse(500, err.Error())
			}
		}
		estimate, err := a.store.onlineCycleCapacity(ctx, account, selected, points, a.cfg, forecastReference(points, isCurrent))
		if err != nil {
			return textResponse(500, err.Error())
		}
		allowances, errAllowance := a.store.remainingModelAllowances(ctx, estimate.RemainingCostUSD, a.cfg)
		if errAllowance != nil {
			return textResponse(500, errAllowance.Error())
		}
		active, taskID := a.pricingTaskActive()
		response := map[string]any{"account": account, "plan_type": plan, "selected_cycle_id": selected.ID, "selected_reset_at": selected.ResetAt, "is_current": isCurrent, "cycle": selected, "points": points, "capacity_points": capacityHistory(points), "range_points": rangePoints, "range_capacity_points": capacityHistoryForCycles(rangePoints), "range_cycles": rangeCycles, "quota_anomalies": anomalies, "range_quota_anomalies": rangeAnomalies, "estimate": estimate, "remaining_by_model": allowances, "pricing_mode": normalizePricingMode(a.cfg.PricingMode), "value_unit": pricingValueUnit(a.cfg.PricingMode), "burn_forecast": burnWithOnlineCapacity(points, forecastReference(points, isCurrent), estimate), "recalculating": active, "pricing_task_id": taskID}
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
			hasSparkWeeklyQuota, errSpark := a.store.hasSparkFiveHourWeeklyQuota(ctx, account)
			if errSpark != nil {
				return textResponse(500, errSpark.Error())
			}
			response["spark_five_hour_quota_detected"] = hasSparkWeeklyQuota
			if hasSparkWeeklyQuota {
				sparkWeeklySeries, errSpark := a.store.latestQuotaScopeSeries(ctx, account, sparkWeeklyQuotaScope, limit)
				if errSpark != nil {
					return textResponse(500, errSpark.Error())
				}
				response["spark_weekly_quota"] = sparkWeeklySeries
			}
		}
		return a.coverageResponse(ctx, account, response)
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
		active, taskID := a.pricingTaskActive()
		response := map[string]any{"account": account, "months": months, "summary": monthly, "pricing_mode": normalizePricingMode(a.cfg.PricingMode), "value_unit": pricingValueUnit(a.cfg.PricingMode), "recalculating": active, "pricing_task_id": taskID}
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
			hasSparkWeeklyQuota, errSpark := a.store.hasSparkFiveHourWeeklyQuota(ctx, account)
			if errSpark != nil {
				return textResponse(500, errSpark.Error())
			}
			response["spark_five_hour_quota_detected"] = hasSparkWeeklyQuota
			if hasSparkWeeklyQuota {
				sparkWeeklyMonthly, errSpark := a.store.monthlyQuotaScope(ctx, account, sparkWeeklyQuotaScope, selectedMonth)
				if errSpark != nil {
					return textResponse(500, errSpark.Error())
				}
				response["spark_weekly_summary"] = sparkWeeklyMonthly
			}
		}
		return a.coverageResponse(ctx, account, response)
	case strings.HasSuffix(req.Path, "/coverage-settings"):
		account := req.Query.Get("account")
		if strings.TrimSpace(account) == "" {
			return textResponse(400, "account is required")
		}
		if strings.EqualFold(req.Method, "POST") {
			var update struct {
				Mode string `json:"mode"`
			}
			if err := json.Unmarshal(req.Body, &update); err != nil || !validCoverageMode(update.Mode) {
				return textResponse(400, "mode must be cpa_only, mixed, or unknown")
			}
			if err := a.store.saveAccountCoverage(ctx, account, update.Mode); err != nil {
				return textResponse(500, err.Error())
			}
		} else if !strings.EqualFold(req.Method, "GET") {
			return textResponse(405, "method not allowed")
		}
		coverage, err := a.store.accountCoverage(ctx, account)
		if err != nil {
			return textResponse(500, err.Error())
		}
		return jsonResponse(200, map[string]any{"account": account, "collection_coverage": coverage})
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
	case strings.HasSuffix(req.Path, "/pricing-settings/task"):
		if !strings.EqualFold(req.Method, "GET") {
			return textResponse(405, "method not allowed")
		}
		task, ok := a.latestPricingTask(req.Query.Get("id"))
		if !ok {
			return textResponse(404, "pricing task not found")
		}
		return jsonResponse(200, task)
	case strings.HasSuffix(req.Path, "/pricing-settings"):
		if strings.EqualFold(req.Method, "GET") {
			settings := pricingSettingsResponse(a.cfg)
			active, id := a.pricingTaskActive()
			settings["recalculating"], settings["pricing_task_id"] = active, id
			return jsonResponse(200, settings)
		}
		if !strings.EqualFold(req.Method, "POST") {
			return textResponse(405, "method not allowed")
		}
		return a.handlePricingSettingsSave(req)
	case strings.HasSuffix(req.Path, "/prices/sync"):
		count, err := syncPrices(ctx, a.store, a.cfg)
		if err != nil {
			return textResponse(502, err.Error())
		}
		go a.refreshPriceCatalog(context.Background(), a.store)
		return jsonResponse(200, map[string]any{"ok": true, "count": count, "source": a.cfg.PriceSourceURL})
	case strings.HasSuffix(req.Path, "/prices"):
		prices, err := a.store.listPrices(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		rows := make([]modelPriceRow, 0, len(prices))
		for _, p := range prices {
			rows = append(rows, a.cfg.priceRow(p))
		}
		active, id := a.pricingTaskActive()
		return jsonResponse(200, map[string]any{"prices": prices, "rows": rows, "settings": pricingSettingsResponse(a.cfg), "recalculating": active, "pricing_task_id": id})
	default:
		return textResponse(404, "not found")
	}
}

func (a *app) handleEarlyResetRepair(req managementRequest) managementResponse {
	a.mu.RLock()
	s := a.store
	a.mu.RUnlock()
	if s == nil {
		return textResponse(503, "plugin store is not ready")
	}
	account := req.Query.Get("account")
	if account == "" {
		return textResponse(400, "account is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	report, err := s.repairFalseEarlyResets(ctx, account, true)
	if err != nil {
		return textResponse(500, err.Error())
	}
	return jsonResponse(200, report)
}

func (a *app) handlePricingSettingsSave(req managementRequest) managementResponse {
	a.mu.RLock()
	s, cfg := a.store, a.cfg
	a.mu.RUnlock()
	if s == nil {
		return textResponse(503, "plugin store is not ready")
	}
	var update pricingSettingsUpdate
	if err := json.Unmarshal(req.Body, &update); err != nil {
		return textResponse(400, "invalid pricing settings: "+err.Error())
	}
	mode := strings.TrimSpace(update.PricingMode)
	if mode == "" {
		mode = cfg.PricingMode
	}
	if !validPricingMode(mode) {
		return textResponse(400, "pricing_mode must be api, credits, or custom")
	}
	settings := cfg.pricingSettings()
	settings.PricingMode = normalizePricingMode(mode)
	if update.AnchorModel != "" {
		settings.AnchorModel = normalizeModel(update.AnchorModel)
	}
	if update.RestoreOfficial {
		settings.CustomPrices = map[string]customModelPrice{}
		settings.CustomFastMultiplier = 2
		settings.CustomLongContext = true
		settings.CustomLongThreshold = 272000
	}
	if update.CustomPrices != nil {
		settings.CustomPrices = update.CustomPrices
	}
	if update.CustomFastMultiplier != nil {
		settings.CustomFastMultiplier = *update.CustomFastMultiplier
	}
	if update.CustomLongContext != nil {
		settings.CustomLongContext = *update.CustomLongContext
	}
	if update.CustomLongThreshold != nil {
		settings.CustomLongThreshold = *update.CustomLongThreshold
	}
	var err error
	settings, err = normalizePricingSettings(settings)
	if err != nil {
		return textResponse(400, err.Error())
	}
	if settings.PricingMode != pricingModeCustom && cfg.baseInputForModel(settings.AnchorModel, settings.PricingMode) <= 0 {
		return textResponse(400, "anchor_model has no official price")
	}
	for model := range settings.CustomPrices {
		if _, ok := cfg.PriceCatalog[model]; !ok {
			return textResponse(400, "unknown custom price model: "+model)
		}
	}
	task, err := a.startPricingTask(s, settings, cfg.withPricingSettings(settings))
	if err != nil {
		return jsonResponse(409, map[string]any{"error": err.Error(), "task_id": task.ID, "status": task.Status})
	}
	return jsonResponse(202, task)
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
	item.Estimate, err = a.store.onlineCycleCapacity(ctx, account, selected, points, a.cfg, forecastReference(points, selected.Current))
	if err != nil {
		return item, err
	}
	item.BurnForecast = burnWithOnlineCapacity(points, forecastReference(points, selected.Current), item.Estimate)
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

func pricingSettingsResponse(cfg config) map[string]any {
	return map[string]any{
		"available_modes":        []string{pricingModeAPI, pricingModeCredits, pricingModeCustom},
		"pricing_mode":           normalizePricingMode(cfg.PricingMode),
		"value_unit":             pricingValueUnit(cfg.PricingMode),
		"anchor_model":           normalizeModel(cfg.AnchorModel),
		"custom_prices":          cfg.CustomPrices,
		"custom_fast_multiplier": cfg.CustomFastMultiplier,
		"custom_long_context":    cfg.CustomLongContext,
		"custom_long_threshold":  cfg.CustomLongThreshold,
		"price_source_url":       cfg.PriceSourceURL,
	}
}
