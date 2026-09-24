package main

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
)

type usageBreakdownRow struct {
	Model             string   `json:"model"`
	ServiceTier       string   `json:"service_tier"`
	Requests          int64    `json:"requests"`
	Failed            int64    `json:"failed"`
	InputTokens       int64    `json:"input_tokens"`
	CacheReadTokens   int64    `json:"cache_read_tokens"`
	CacheWriteTokens  int64    `json:"cache_write_tokens"`
	OutputTokens      int64    `json:"output_tokens"`
	TotalTokens       int64    `json:"total_tokens"`
	CurrentValue      float64  `json:"current_value"`
	OfficialCredits   *float64 `json:"official_credits,omitempty"`
	LearnedQuotaPct   *float64 `json:"learned_quota_pct,omitempty"`
	EstimatedQuotaPct *float64 `json:"estimated_quota_pct,omitempty"`
}

type usageBreakdown struct {
	Account               string              `json:"account"`
	StartAt               int64               `json:"start_at"`
	EndAt                 int64               `json:"end_at"`
	Days                  int                 `json:"days"`
	PricingMode           string              `json:"pricing_mode"`
	ValueUnit             string              `json:"value_unit"`
	Rows                  []usageBreakdownRow `json:"rows"`
	Requests              int64               `json:"requests"`
	Failed                int64               `json:"failed"`
	TotalTokens           int64               `json:"total_tokens"`
	CurrentValue          float64             `json:"current_value"`
	OfficialCredits       float64             `json:"official_credits"`
	UnpricedRequests      int64               `json:"unpriced_requests"`
	QuotaGrowthPercent    float64             `json:"quota_growth_percent"`
	QuotaCoverageComplete bool                `json:"quota_coverage_complete"`
	LearnedQuotaPct       float64             `json:"learned_quota_pct"`
	EstimatedQuotaPct     float64             `json:"estimated_quota_pct"`
	EstimatedVsActualPct  float64             `json:"estimated_vs_actual_pct"`
	UnattributedRequests  int64               `json:"unattributed_requests"`
}

func officialCreditsForUsage(model, tier string, input, cached, written, output int64) (float64, bool) {
	p, ok := officialCodexCreditPrice(model)
	if !ok {
		return 0, false
	}
	multiplier := 1.0
	if isFastTier(tier) {
		switch normalizeModel(model) {
		case "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5":
			multiplier = 2.5
		case "gpt-5.4":
			multiplier = 2
		default:
			return 0, false
		}
	} else if tier != "" && tier != "auto" && tier != "default" && tier != "standard" {
		return 0, false
	}
	uncached := input - cached - written
	if uncached < 0 {
		uncached = 0
	}
	// Codex has no separate credit charge for cache writes.
	return (float64(uncached)*p.Input + float64(cached)*p.CacheRead + float64(output)*p.Output) * multiplier / 1_000_000, true
}

func (s *store) usageBreakdown(ctx context.Context, account string, startAt, endAt int64, days int, cfg config) (usageBreakdown, error) {
	result := usageBreakdown{
		Account: account, StartAt: startAt, EndAt: endAt, Days: days,
		PricingMode: normalizePricingMode(cfg.PricingMode), ValueUnit: pricingValueUnit(cfg.PricingMode),
		Rows: []usageBreakdownRow{}, QuotaCoverageComplete: true,
	}
	if endAt <= startAt {
		return result, fmt.Errorf("valid time range is required")
	}
	if account == "" {
		return result, nil
	}
	cycles, err := s.cycles(ctx, account, 1000)
	if err != nil {
		return result, err
	}
	valuePerPercent := make(map[int64]float64, len(cycles))
	states, _, err := s.accountOnlineScales(ctx, account, cfg, endAt)
	if err != nil {
		return result, err
	}
	ordered := append([]quotaCycle(nil), cycles...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].StartedAt < ordered[j].StartedAt })
	priorValuePerPercent := float64(0)
	for _, cycle := range ordered {
		if snapshot, ok := states[cycle.ID]; ok && snapshot.State != nil {
			value := 1 / math.Exp(snapshot.State.LogValue)
			if isFinitePositive(value) {
				priorValuePerPercent = value
			}
		}
		if cycle.StartedAt >= endAt || cycle.EndedAt > 0 && cycle.EndedAt <= startAt {
			continue
		}
		if priorValuePerPercent > 0 {
			valuePerPercent[cycle.ID] = priorValuePerPercent
			continue
		}
		points, _, errPoints := s.pointsForCycle(ctx, account, cycle.ID, 10000)
		if errPoints != nil {
			return result, errPoints
		}
		if estimate := estimateCapacity(points); estimate.FullWindowCostUSD > 0 {
			valuePerPercent[cycle.ID] = estimate.FullWindowCostUSD / 100
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT cycle_id,model,service_tier,COUNT(*),COALESCE(SUM(failed),0),
		COALESCE(SUM(input_tokens),0),COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cache_write_tokens),0),
		COALESCE(SUM(output_tokens),0),COALESCE(SUM(total_tokens),0),COALESCE(SUM(cost_usd),0),
		SUM(learned_quota_pct),COUNT(learned_quota_pct)
		FROM usage_events WHERE account=? AND quota_scope=? AND requested_at>=? AND requested_at<?
		GROUP BY cycle_id,model,service_tier`, account, mainQuotaScope, startAt, endAt)
	if err != nil {
		return result, err
	}
	byKey := make(map[string]*usageBreakdownRow)
	for rows.Next() {
		var row usageBreakdownRow
		var cycleID int64
		var learned sql.NullFloat64
		var attributed int64
		if err = rows.Scan(&cycleID, &row.Model, &row.ServiceTier, &row.Requests, &row.Failed,
			&row.InputTokens, &row.CacheReadTokens, &row.CacheWriteTokens,
			&row.OutputTokens, &row.TotalTokens, &row.CurrentValue, &learned, &attributed); err != nil {
			rows.Close()
			return result, err
		}
		row.Model = normalizeModel(row.Model)
		row.ServiceTier = strings.ToLower(strings.TrimSpace(row.ServiceTier))
		if learned.Valid {
			v := learned.Float64
			row.LearnedQuotaPct = &v
		}
		if perPercent := valuePerPercent[cycleID]; perPercent > 0 {
			value := row.CurrentValue / perPercent
			row.EstimatedQuotaPct = &value
		}
		result.UnattributedRequests += row.Requests - attributed
		key := row.Model + "\x00" + row.ServiceTier
		if existing := byKey[key]; existing != nil {
			existing.Requests += row.Requests
			existing.Failed += row.Failed
			existing.InputTokens += row.InputTokens
			existing.CacheReadTokens += row.CacheReadTokens
			existing.CacheWriteTokens += row.CacheWriteTokens
			existing.OutputTokens += row.OutputTokens
			existing.TotalTokens += row.TotalTokens
			existing.CurrentValue += row.CurrentValue
			if row.EstimatedQuotaPct != nil {
				if existing.EstimatedQuotaPct == nil {
					v := *row.EstimatedQuotaPct
					existing.EstimatedQuotaPct = &v
				} else {
					*existing.EstimatedQuotaPct += *row.EstimatedQuotaPct
				}
			}
			if row.LearnedQuotaPct != nil {
				if existing.LearnedQuotaPct == nil {
					v := *row.LearnedQuotaPct
					existing.LearnedQuotaPct = &v
				} else {
					*existing.LearnedQuotaPct += *row.LearnedQuotaPct
				}
			}
		} else {
			copy := row
			byKey[key] = &copy
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	if err = rows.Close(); err != nil {
		return result, err
	}
	for _, row := range byKey {
		if credits, ok := officialCreditsForUsage(row.Model, row.ServiceTier, row.InputTokens, row.CacheReadTokens, row.CacheWriteTokens, row.OutputTokens); ok {
			row.OfficialCredits = &credits
			result.OfficialCredits += credits
		} else {
			result.UnpricedRequests += row.Requests
		}
		result.Requests += row.Requests
		result.Failed += row.Failed
		result.TotalTokens += row.TotalTokens
		result.CurrentValue += row.CurrentValue
		if row.LearnedQuotaPct != nil {
			result.LearnedQuotaPct += *row.LearnedQuotaPct
		}
		if row.EstimatedQuotaPct != nil {
			result.EstimatedQuotaPct += *row.EstimatedQuotaPct
		}
		result.Rows = append(result.Rows, *row)
	}
	sort.Slice(result.Rows, func(i, j int) bool {
		if result.Rows[i].CurrentValue != result.Rows[j].CurrentValue {
			return result.Rows[i].CurrentValue > result.Rows[j].CurrentValue
		}
		if result.Rows[i].Model != result.Rows[j].Model {
			return result.Rows[i].Model < result.Rows[j].Model
		}
		return result.Rows[i].ServiceTier < result.Rows[j].ServiceTier
	})
	for _, cycle := range cycles {
		if cycle.StartedAt >= endAt || cycle.EndedAt > 0 && cycle.EndedAt <= startAt {
			continue
		}
		growth, complete, errGrowth := s.cycleQuotaGrowth(ctx, cycle, startAt, endAt)
		if errGrowth != nil {
			return result, errGrowth
		}
		result.QuotaGrowthPercent += growth
		result.QuotaCoverageComplete = result.QuotaCoverageComplete && complete
	}
	result.EstimatedVsActualPct = result.EstimatedQuotaPct - result.QuotaGrowthPercent
	return result, nil
}
