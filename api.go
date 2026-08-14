package main

import (
	"context"
	"database/sql"
	_ "embed"
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
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.store == nil {
		return textResponse(503, "plugin store is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		resp := map[string]any{"plugin_version": pluginVersion, "account": account, "accounts": accounts, "config": map[string]any{"fast_pricing_mode": a.cfg.FastPricingMode, "fast_multiplier": a.cfg.FastMultiplier, "long_context_threshold": a.cfg.LongContextThreshold, "price_source_url": a.cfg.PriceSourceURL}}
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
		return jsonResponse(200, map[string]any{"account": account, "plan_type": plan, "selected_cycle_id": selected.ID, "selected_reset_at": selected.ResetAt, "is_current": isCurrent, "cycle": selected, "points": points, "capacity_points": capacityHistory(points), "estimate": estimateCapacity(points), "burn_forecast": estimateBurn(points, forecastReference(points, isCurrent))})
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
		return jsonResponse(200, map[string]any{"account": account, "months": months, "summary": monthly})
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
		return jsonResponse(200, map[string]any{"prices": prices, "fast_pricing_mode": a.cfg.FastPricingMode, "fast_multiplier": a.cfg.FastMultiplier})
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
