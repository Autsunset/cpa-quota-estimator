package main

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func coverageRequest(t *testing.T, a *app, method, endpoint, account, body string) map[string]any {
	t.Helper()
	response := a.handleManagement(managementRequest{Method: method, Path: "/cpa-quota-estimator/" + endpoint,
		Query: url.Values{"account": {account}, "include_spark": {"1"}}, Body: []byte(body)})
	if response.StatusCode != 200 {
		t.Fatalf("%s %s: status=%d body=%s", method, endpoint, response.StatusCode, response.Body)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func seedCoverageUsage(t *testing.T, s *store, account string, extraUntrackedUsage, dualScopes bool) {
	t.Helper()
	now := time.Now().Unix()
	_, monthStart, _, err := monthRange("")
	if err != nil {
		t.Fatal(err)
	}
	// Keep every synthetic request in the selected month, including runs
	// during its first few minutes.
	now = max(now, monthStart+600)
	window := int64(10080)
	reset := now + 3*24*60*60
	if dualScopes {
		window = 300
		reset = now + 60*60
	}
	for i := 0; i <= 6; i++ {
		used := 20 + float64(i)
		if extraUntrackedUsage {
			used = 20 + float64(i)*2
		}
		tokens, cost := int64(100000), 1.0
		if i == 0 {
			tokens, cost = 0, 0
		}
		e := event{Account: account, RequestedAt: now - 420 + int64(i)*60,
			UsedPercent: &used, ResetAt: reset, WindowMinutes: window, PlanType: "pro",
			Model: "gpt-5.5", TotalTokens: tokens, CostUSD: cost}
		if dualScopes {
			e.SecondaryUsedPercent = &used
			e.SecondaryResetAt = now + 3*24*60*60
			e.SecondaryWindowMinutes = 10080
		}
		if err := s.insertEvent(context.Background(), e, time.Minute); err != nil {
			t.Fatal(err)
		}
		if dualScopes {
			e.QuotaScope, e.Model = sparkQuotaScope, "gpt-5.3-codex-spark"
			if err := s.insertEvent(context.Background(), e, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.upsertPrices(context.Background(), []price{{Model: "gpt-5.5", Input: 1, Output: 2, CacheRead: .1}}); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageDefaultPreservesEstimatesWithExplicitAssumption(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "coverage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}
	seedCoverageUsage(t, s, "cpa-only", false, false)
	seedCoverageUsage(t, s, "mixed", true, false)
	for account, fullTokens := range map[string]float64{"cpa-only": 10000000, "mixed": 5000000} {
		payload := coverageRequest(t, a, "GET", "summary", account, "")
		coverage, ok := payload["collection_coverage"].(map[string]any)
		if !ok || coverage["mode"] != "cpa_only" || coverage["configured"] != false ||
			coverage["assumption"] != "all_usage_through_cpa" || coverage["usage_source"] != "cpa" || coverage["quota_source"] != "account_quota_pool" {
			t.Fatalf("default coverage must state its assumption: %#v", coverage)
		}
		estimate := payload["estimate"].(map[string]any)
		if estimate["available"] != true || estimate["full_window_tokens"] != fullTokens ||
			estimate["sample_confidence"] != "high" || estimate["coverage_mode"] != "cpa_only" || estimate["assumption"] != "all_usage_through_cpa" {
			t.Fatalf("existing estimate changed or omitted its coverage assumption: %#v", estimate)
		}
	}
}

func TestCoverageModesSuppressCapacityWithoutChangingTheLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.close() }()
	ctx := context.Background()
	a := &app{cfg: defaultConfig(), store: s}
	seedCoverageUsage(t, s, "mixed", true, false)
	seedCoverageUsage(t, s, "unaffected", false, false)
	baseline := coverageRequest(t, a, "GET", "summary", "mixed", "")
	baselineCycles, err := s.cycles(ctx, "mixed", 10)
	if err != nil {
		t.Fatal(err)
	}
	// The persistence check below expects the last saved mode to be unknown.
	// Map iteration order cannot define this sequence.
	for _, scenario := range []struct{ mode, reason string }{
		{"mixed", "partial_usage_collection"},
		{"unknown", "usage_coverage_unknown"},
	} {
		mode, reason := scenario.mode, scenario.reason
		settings := coverageRequest(t, a, "POST", "coverage-settings", "mixed", `{"mode":"`+mode+`"}`)
		if settings["collection_coverage"].(map[string]any)["configured"] != true {
			t.Fatal("saved coverage was not marked as configured")
		}
		for _, endpoint := range []string{"summary", "series"} {
			payload := coverageRequest(t, a, "GET", endpoint, "mixed", "")
			estimate := payload["estimate"].(map[string]any)
			if estimate["available"] != false || estimate["confidence"] != "unavailable" ||
				estimate["sample_confidence"] != "high" || estimate["unavailable_reason"] != reason || estimate["sample_count"].(float64) < 5 {
				t.Fatalf("%s %s confuses sufficient sampling with coverage: %#v", mode, endpoint, estimate)
			}
			assertNoCoverageCapacity(t, payload)
			if payload["burn_forecast"].(map[string]any)["available"] != true {
				t.Fatal("quota percentage trends must remain available")
			}
			if len(payload["remaining_by_model"].([]any)) != 0 {
				t.Fatal("model allowances leaked a partial-coverage estimate")
			}
			if endpoint == "summary" && !reflect.DeepEqual(payload["latest"], baseline["latest"]) {
				t.Fatal("coverage changed actual usage or quota observations")
			}
			if endpoint == "series" && (len(payload["capacity_points"].([]any)) != 0 || len(payload["range_capacity_points"].([]any)) != 0 || len(payload["points"].([]any)) == 0) {
				t.Fatal("capacity history must be hidden while actual observations remain available")
			}
		}
		monthly := coverageRequest(t, a, "GET", "monthly", "mixed", "")
		assertNoCoverageCapacity(t, monthly)
		if monthly["summary"].(map[string]any)["actual_tokens"] != float64(600000) {
			t.Fatal("monthly actual CPA Tokens changed")
		}
		overview := coverageRequest(t, a, "GET", "overview", "", "")
		for _, value := range overview["accounts"].([]any) {
			item := value.(map[string]any)
			if item["account"] == "mixed" {
				assertNoCoverageCapacity(t, item)
			} else if item["estimate"].(map[string]any)["available"] != true {
				t.Fatal("one account's mode changed another account")
			}
		}
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a.store = s
	settings := coverageRequest(t, a, "GET", "coverage-settings", "mixed", "")
	if settings["collection_coverage"].(map[string]any)["mode"] != "unknown" {
		t.Fatalf("coverage setting did not survive restart: %#v", settings["collection_coverage"])
	}
	coverageRequest(t, a, "POST", "coverage-settings", "mixed", `{"mode":"cpa_only"}`)
	restored := coverageRequest(t, a, "GET", "summary", "mixed", "")
	if !reflect.DeepEqual(baseline["estimate"], restored["estimate"]) {
		t.Fatalf("restoring the assumption changed the estimate: before=%#v after=%#v", baseline["estimate"], restored["estimate"])
	}
	currentCycles, err := s.cycles(ctx, "mixed", 10)
	if err != nil || !reflect.DeepEqual(baselineCycles, currentCycles) {
		t.Fatalf("coverage settings changed cycle accounting: cycles=%#v err=%v", currentCycles, err)
	}
}

func TestCoverageAppliesToIndependentWeeklyAndSparkScopes(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "scopes.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}
	seedCoverageUsage(t, s, "mixed", true, true)
	coverageRequest(t, a, "POST", "coverage-settings", "mixed", `{"mode":"mixed"}`)
	series := coverageRequest(t, a, "GET", "series", "mixed", "")
	for _, key := range []string{"weekly_quota", "spark_quota", "spark_weekly_quota"} {
		scope, ok := series[key].(map[string]any)
		if !ok {
			t.Fatalf("missing independent quota scope %s", key)
		}
		if scope["estimate"].(map[string]any)["available"] != false || len(scope["points"].([]any)) == 0 || len(scope["capacity_points"].([]any)) != 0 {
			t.Fatalf("%s did not separate quota observations from capacity: %#v", key, scope)
		}
	}
	assertNoCoverageCapacity(t, series)
	monthly := coverageRequest(t, a, "GET", "monthly", "mixed", "")
	for _, key := range []string{"summary", "weekly_summary", "spark_summary", "spark_weekly_summary"} {
		if _, ok := monthly[key]; !ok {
			t.Fatalf("missing independent monthly summary %s", key)
		}
	}
	assertNoCoverageCapacity(t, monthly)
}

func assertNoCoverageCapacity(t *testing.T, value any) {
	t.Helper()
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			switch key {
			case "full_window_tokens", "full_window_cost_usd", "remaining_tokens", "remaining_cost_usd", "token_low", "token_high", "cost_low", "cost_high", "estimated_tokens", "estimated_token_low", "estimated_token_high", "estimated_cost_usd", "estimated_cost_low", "estimated_cost_high":
				if child != nil {
					t.Fatalf("%s must be null when collection coverage blocks estimation, got %#v", key, child)
				}
			}
			assertNoCoverageCapacity(t, child)
		}
	case []any:
		for _, child := range node {
			assertNoCoverageCapacity(t, child)
		}
	}
}

func TestCoverageSettingsRejectInvalidRequests(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "invalid.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}
	for _, tc := range []struct{ method, account, body string }{
		{"POST", "", `{"mode":"mixed"}`}, {"POST", "account", `{"mode":"full"}`},
		{"POST", "account", `{}`}, {"POST", "account", `not-json`}, {"GET", "", ""},
	} {
		response := a.handleManagement(managementRequest{Method: tc.method, Path: "/cpa-quota-estimator/coverage-settings",
			Query: url.Values{"account": {tc.account}}, Body: []byte(tc.body)})
		if response.StatusCode != 400 {
			t.Fatalf("invalid coverage request: status=%d body=%s", response.StatusCode, response.Body)
		}
	}
}
