package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	coverageCPAOnly = "cpa_only"
	coverageMixed   = "mixed"
	coverageUnknown = "unknown"
	coverageKey     = "collection_coverage:"
)

type collectionCoverage struct {
	Mode                      string `json:"mode"`
	Configured                bool   `json:"configured"`
	UsageSource               string `json:"usage_source"`
	QuotaSource               string `json:"quota_source"`
	CapacityEstimationEnabled bool   `json:"capacity_estimation_enabled"`
	Assumption                string `json:"assumption,omitempty"`
	UnavailableReason         string `json:"unavailable_reason,omitempty"`
}

func validCoverageMode(mode string) bool {
	return mode == coverageCPAOnly || mode == coverageMixed || mode == coverageUnknown
}

func coverageForMode(mode string, configured bool) collectionCoverage {
	coverage := collectionCoverage{Mode: mode, Configured: configured, UsageSource: "cpa", QuotaSource: "account_quota_pool"}
	switch mode {
	case coverageCPAOnly:
		coverage.CapacityEstimationEnabled = true
		coverage.Assumption = "all_usage_through_cpa"
	case coverageMixed:
		coverage.UnavailableReason = "partial_usage_collection"
	default:
		coverage.UnavailableReason = "usage_coverage_unknown"
	}
	return coverage
}

func (s *store) accountCoverage(ctx context.Context, account string) (collectionCoverage, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key=?`, coverageKey+account).Scan(&mode)
	if err == sql.ErrNoRows {
		// Preserve existing estimates, but declare their coverage assumption.
		return coverageForMode(coverageCPAOnly, false), nil
	}
	if err != nil {
		return collectionCoverage{}, err
	}
	if !validCoverageMode(mode) {
		return collectionCoverage{}, fmt.Errorf("invalid saved collection coverage")
	}
	return coverageForMode(mode, true), nil
}

func (s *store) saveAccountCoverage(ctx context.Context, account, mode string) error {
	if strings.TrimSpace(account) == "" || !validCoverageMode(mode) {
		return fmt.Errorf("account and a valid coverage mode are required")
	}
	// This preference changes presentation only. Never recalculate usage,
	// prices, samples or cycle boundaries when saving collection coverage.
	_, err := s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, coverageKey+account, mode)
	return err
}

func estimateForCoverage(value estimate, coverage collectionCoverage) estimate {
	value.CoverageMode = coverage.Mode
	value.SampleConfidence = value.Confidence
	value.Assumption = coverage.Assumption
	if coverage.CapacityEstimationEnabled {
		value.Explanation += " 假设该额度池的全部用量经过 CPA；采样充分程度不代表采集覆盖完整。"
		return value
	}
	explanation := "仅采集部分入口，无法将 CPA 用量换算为整个账号的完整周期或剩余容量。"
	if coverage.Mode == coverageUnknown {
		explanation = "采集覆盖范围未知，无法将 CPA 用量换算为整个账号的完整周期或剩余容量。"
	}
	return estimate{CoverageMode: coverage.Mode, SampleConfidence: value.Confidence,
		SampleCount: value.SampleCount, PercentSpan: value.PercentSpan,
		Confidence: "unavailable", UnavailableReason: coverage.UnavailableReason, Explanation: explanation}
}

func scopeSeriesForCoverage(value scopedQuotaSeries, coverage collectionCoverage) scopedQuotaSeries {
	value.CollectionCoverage = &coverage
	value.Estimate = estimateForCoverage(value.Estimate, coverage)
	if !coverage.CapacityEstimationEnabled {
		value.CapacityPoints = []capacityPoint{}
		value.RemainingByModel = []modelAllowance{}
	}
	return value
}

func monthlyForCoverage(value monthlySummary, coverage collectionCoverage) monthlySummary {
	value.CollectionCoverage = &coverage
	value.Cycles = append([]monthlyCycle(nil), value.Cycles...)
	for index := range value.Cycles {
		cycle := &value.Cycles[index]
		cycle.CoverageMode = coverage.Mode
		cycle.SampleConfidence = cycle.Confidence
		if !coverage.CapacityEstimationEnabled {
			cycle.CapacityAvailable = false
			cycle.Confidence = "unavailable"
			cycle.UnavailableReason = coverage.UnavailableReason
			cycle.FullWindowTokens, cycle.FullWindowCostUSD = 0, 0
			cycle.TokenLow, cycle.TokenHigh, cycle.CostLow, cycle.CostHigh = 0, 0, 0, 0
		}
	}
	if !coverage.CapacityEstimationEnabled {
		value.UnavailableReason = coverage.UnavailableReason
		value.EstimatedCycleCount = 0
		value.EstimatedTokens, value.EstimatedTokenLow, value.EstimatedTokenHigh = 0, 0, 0
		value.EstimatedCostUSD, value.EstimatedCostLow, value.EstimatedCostHigh = 0, 0, 0
	}
	return value
}

// Apply the account's policy to every capacity surface in a response, including
// historical charts and the independent weekly/Spark axes. Accounting stays in
// the store; restricted values cannot leak through an alternate API endpoint.
func (a *app) coverageResponse(ctx context.Context, account string, payload map[string]any) managementResponse {
	coverage, err := a.store.accountCoverage(ctx, account)
	if err != nil {
		return textResponse(500, err.Error())
	}
	payload["collection_coverage"] = coverage
	for key, item := range payload {
		switch value := item.(type) {
		case estimate:
			payload[key] = estimateForCoverage(value, coverage)
		case scopedQuotaSeries:
			payload[key] = scopeSeriesForCoverage(value, coverage)
		case monthlySummary:
			payload[key] = monthlyForCoverage(value, coverage)
		}
	}
	if !coverage.CapacityEstimationEnabled {
		for _, key := range []string{"capacity_points", "range_capacity_points"} {
			if _, exists := payload[key]; exists {
				payload[key] = []capacityPoint{}
			}
		}
		if _, exists := payload["remaining_by_model"]; exists {
			payload["remaining_by_model"] = []modelAllowance{}
		}
	}
	return jsonResponse(200, payload)
}

// Keep numeric JSON unchanged for existing/default clients. A coverage-blocked
// capacity is null, not zero; RawMessage preserves integer precision in the
// accounting fields while replacing only the unavailable estimates.
func marshalNullCapacity(value any, fields ...string) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	for _, field := range fields {
		object[field] = json.RawMessage("null")
	}
	return json.Marshal(object)
}

func (value estimate) MarshalJSON() ([]byte, error) {
	type plainEstimate estimate
	if value.CoverageMode == "" || value.CoverageMode == coverageCPAOnly {
		return json.Marshal(plainEstimate(value))
	}
	return marshalNullCapacity(plainEstimate(value), "full_window_tokens", "full_window_cost_usd", "token_low", "token_high", "cost_low", "cost_high", "remaining_tokens", "remaining_cost_usd")
}

func (value monthlyCycle) MarshalJSON() ([]byte, error) {
	type plainMonthlyCycle monthlyCycle
	if value.CoverageMode == "" || value.CoverageMode == coverageCPAOnly {
		return json.Marshal(plainMonthlyCycle(value))
	}
	return marshalNullCapacity(plainMonthlyCycle(value), "full_window_tokens", "full_window_cost_usd", "token_low", "token_high", "cost_low", "cost_high")
}

func (value monthlySummary) MarshalJSON() ([]byte, error) {
	type plainMonthlySummary monthlySummary
	if value.CollectionCoverage == nil || value.CollectionCoverage.CapacityEstimationEnabled {
		return json.Marshal(plainMonthlySummary(value))
	}
	return marshalNullCapacity(plainMonthlySummary(value), "estimated_tokens", "estimated_token_low", "estimated_token_high", "estimated_cost_usd", "estimated_cost_low", "estimated_cost_high")
}
