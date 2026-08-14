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
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.store == nil {
		return textResponse(503, "plugin store is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch {
	case strings.HasSuffix(req.Path, "/summary"):
		tokenMode, err := a.store.metadata(ctx, "token_count_mode")
		if err != nil {
			return textResponse(500, err.Error())
		}
		account := req.Query.Get("account")
		accounts, err := a.store.accounts(ctx)
		if err != nil {
			return textResponse(500, err.Error())
		}
		if account == "" && len(accounts) > 0 {
			account = accounts[0]
		}
		resp := map[string]any{"plugin_version": pluginVersion, "account": account, "accounts": accounts, "config": map[string]any{"fast_pricing_mode": a.cfg.FastPricingMode, "fast_multiplier": a.cfg.FastMultiplier, "long_context_threshold": a.cfg.LongContextThreshold, "price_source_url": a.cfg.PriceSourceURL, "token_count_mode": tokenMode}}
		if account != "" {
			points, plan, err := a.store.latestPoints(ctx, account, 5000, tokenMode)
			if err != nil && err != sql.ErrNoRows {
				return textResponse(500, err.Error())
			}
			resp["plan_type"] = plan
			resp["estimate"] = estimateCapacity(points)
			if len(points) > 0 {
				resp["latest"] = points[len(points)-1]
				resp["window_start"] = points[len(points)-1].ResetAt - points[len(points)-1].WindowMinutes*60
			}
		}
		return jsonResponse(200, resp)
	case strings.HasSuffix(req.Path, "/series"):
		tokenMode, err := a.store.metadata(ctx, "token_count_mode")
		if err != nil {
			return textResponse(500, err.Error())
		}
		account := req.Query.Get("account")
		if account == "" {
			accounts, _ := a.store.accounts(ctx)
			if len(accounts) > 0 {
				account = accounts[0]
			}
		}
		limit, _ := strconv.Atoi(req.Query.Get("limit"))
		points, plan, err := a.store.latestPoints(ctx, account, limit, tokenMode)
		if err != nil && err != sql.ErrNoRows {
			return textResponse(500, err.Error())
		}
		return jsonResponse(200, map[string]any{"account": account, "plan_type": plan, "token_count_mode": tokenMode, "points": points, "capacity_points": capacityHistory(points), "estimate": estimateCapacity(points)})
	case strings.HasSuffix(req.Path, "/settings"):
		if strings.EqualFold(req.Method, "POST") {
			var payload struct {
				TokenCountMode string `json:"token_count_mode"`
			}
			if err := json.Unmarshal(req.Body, &payload); err != nil {
				return textResponse(400, "invalid JSON body")
			}
			mode := normalizeTokenMode(payload.TokenCountMode)
			if mode == "" {
				return textResponse(400, "token_count_mode must be standard or include_cache_read")
			}
			if err := a.store.setMetadata(ctx, "token_count_mode", mode); err != nil {
				return textResponse(500, err.Error())
			}
			return jsonResponse(200, map[string]any{"token_count_mode": mode})
		}
		mode, err := a.store.metadata(ctx, "token_count_mode")
		if err != nil {
			return textResponse(500, err.Error())
		}
		return jsonResponse(200, map[string]any{"token_count_mode": mode})
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
