package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestDashboardShowsRemainingQuotaAndExhaustedState(t *testing.T) {
	for _, expected := range [][]byte{
		[]byte("当前剩余额度"),
		[]byte("overviewExhausted"),
		[]byte("overviewAwaitingFirstSample"),
		[]byte("loadConfiguredCodexAccounts"),
		[]byte("accountMatchKey"),
		[]byte("overviewWeeklyQuota"),
		[]byte("overviewDisabled"),
		[]byte("overviewCapacityCost"),
		[]byte("overviewToggle"),
		[]byte("data-overview-filter"),
		[]byte("compareOverviewRows"),
		[]byte("cqe-overview-preferences-v1"),
		[]byte("setupOverviewColumnResizers"),
		[]byte("overview-column-resizer"),
		[]byte("overview-toggle-icon"),
		[]byte("quotaAnomalyPanel"),
		[]byte("renderQuotaAnomalies"),
		[]byte("quotaAnomalyOverlays"),
		[]byte("quotaAnomalyVisualPoints"),
		[]byte("quotaAnomalyMiniChart"),
		[]byte("quotaAnomalySpikePaths"),
		[]byte("上游额度状态异常"),
	} {
		if !bytes.Contains(dashboardHTML, expected) {
			t.Fatalf("dashboard is missing %q", expected)
		}
	}
}

func TestManagementRegistrationIncludesAllHandledRoutes(t *testing.T) {
	registration := managementRegistration().(map[string]any)
	routes := registration["routes"].([]map[string]any)
	registered := make(map[string]bool, len(routes))
	for _, route := range routes {
		registered[route["Method"].(string)+" "+route["Path"].(string)] = true
	}

	for _, expected := range []string{
		"GET /cpa-quota-estimator/overview",
		"GET /cpa-quota-estimator/summary",
		"GET /cpa-quota-estimator/series",
		"GET /cpa-quota-estimator/monthly",
		"GET /cpa-quota-estimator/repair/early-resets",
		"POST /cpa-quota-estimator/repair/early-resets",
		"GET /cpa-quota-estimator/prices",
		"POST /cpa-quota-estimator/prices/sync",
		"GET /cpa-quota-estimator/pricing-settings",
		"POST /cpa-quota-estimator/pricing-settings",
		"GET /cpa-quota-estimator/coverage-settings",
		"POST /cpa-quota-estimator/coverage-settings",
	} {
		if !registered[expected] {
			t.Fatalf("management route %q is handled but not registered", expected)
		}
	}
}

func TestRecordUsageStoresQuotaObservationTime(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "observed-at.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}
	requestedAt := time.Unix(100, 0)
	record := usageRecord{
		Provider:    "openai",
		Model:       "gpt",
		AuthID:      "observed-account",
		RequestedAt: requestedAt,
		TTFT:        int64(90 * time.Second),
		Detail:      usageDetail{TotalTokens: 100},
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Used-Percent":   {"40"},
			"X-Codex-Primary-Reset-At":       {"700"},
			"X-Codex-Primary-Window-Minutes": {"10"},
			"X-Codex-Plan-Type":              {"pro"},
		},
	}
	if err = a.recordUsage(record); err != nil {
		t.Fatal(err)
	}
	var requested, observed int64
	if err = s.db.QueryRow(`SELECT requested_at,observed_at FROM usage_events LIMIT 1`).Scan(&requested, &observed); err != nil {
		t.Fatal(err)
	}
	if requested != 100 || observed != 190 {
		t.Fatalf("requested=%d observed=%d", requested, observed)
	}
}

func TestPricingSettingsManagementSavesBothSwitches(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "pricing-api.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}

	response := a.handleManagement(managementRequest{
		Method: "POST",
		Path:   "/cpa-quota-estimator/pricing-settings",
		Body:   []byte(`{"apply_long_context_pricing":false,"apply_fast_pricing":false}`),
	})
	if response.StatusCode != 200 {
		t.Fatalf("status = %d, body = %s", response.StatusCode, response.Body)
	}
	if a.cfg.ApplyLongContextPricing || a.cfg.ApplyFastPricing {
		t.Fatalf("app settings = long:%v fast:%v, want both disabled", a.cfg.ApplyLongContextPricing, a.cfg.ApplyFastPricing)
	}
	if a.cfg.PricingMode != pricingModeLegacyAPI {
		t.Fatalf("pricing mode = %q, want pre-discount API", a.cfg.PricingMode)
	}
	settings, err := s.loadPricingSettings(context.Background(), pricingSettings{ApplyLongContext: true, ApplyFast: true})
	if err != nil {
		t.Fatal(err)
	}
	if settings.ApplyLongContext || settings.ApplyFast {
		t.Fatalf("stored settings = %#v, want both disabled", settings)
	}
}

func TestRecordUsageStoresWeeklySecondaryQuotaOnlyForFiveHourPrimary(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "secondary-quota.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}

	record := func(account string, primaryWindow int64) {
		t.Helper()
		if err := a.recordUsage(usageRecord{
			Provider:    "openai",
			Model:       "gpt",
			AuthID:      account,
			RequestedAt: time.Unix(1_000, 0),
			Detail:      usageDetail{TotalTokens: 100},
			ResponseHeaders: http.Header{
				"X-Codex-Primary-Used-Percent":     {"12"},
				"X-Codex-Primary-Reset-At":         {"19001"},
				"X-Codex-Primary-Window-Minutes":   {stringInt(primaryWindow)},
				"X-Codex-Secondary-Used-Percent":   {"34"},
				"X-Codex-Secondary-Reset-At":       {"605801"},
				"X-Codex-Secondary-Window-Minutes": {"10080"},
				"X-Codex-Plan-Type":                {"plus"},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	record("five-hour-account", 300)
	record("weekly-only-account", 10080)

	var used sql.NullFloat64
	var resetAt, windowMinutes int64
	if err = s.db.QueryRow(`SELECT secondary_used_percent,secondary_reset_at,secondary_window_minutes FROM usage_events WHERE account=?`, "five-hour-account").Scan(&used, &resetAt, &windowMinutes); err != nil {
		t.Fatal(err)
	}
	if !used.Valid || used.Float64 != 34 || resetAt != 605_820 || windowMinutes != 10080 {
		t.Fatalf("stored secondary quota = used:%#v reset:%d window:%d", used, resetAt, windowMinutes)
	}
	if err = s.db.QueryRow(`SELECT secondary_used_percent,secondary_reset_at,secondary_window_minutes FROM usage_events WHERE account=?`, "weekly-only-account").Scan(&used, &resetAt, &windowMinutes); err != nil {
		t.Fatal(err)
	}
	if used.Valid || resetAt != 0 || windowMinutes != 0 {
		t.Fatalf("weekly-only account unexpectedly enabled secondary quota = used:%#v reset:%d window:%d", used, resetAt, windowMinutes)
	}
}

func stringInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestManagementExposesWeeklyQuotaOnlyForDetectedFiveHourAccount(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "weekly-api-gating.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()

	primary, secondary := 10.0, 20.0
	plus := event{
		RequestedAt: 1_000, Account: "plus-five-hour", Provider: "openai", Model: "gpt", TotalTokens: 100,
		UsedPercent: &primary, ResetAt: 19_000, WindowMinutes: 300,
		SecondaryUsedPercent: &secondary, SecondaryResetAt: 605_800, SecondaryWindowMinutes: 10080,
		PlanType: "plus",
	}
	if err = s.insertEvent(ctx, plus, time.Minute); err != nil {
		t.Fatal(err)
	}
	pro := event{
		RequestedAt: 1_000, Account: "pro-weekly-only", Provider: "openai", Model: "gpt", TotalTokens: 100,
		UsedPercent: &primary, ResetAt: 605_800, WindowMinutes: 10080,
		SecondaryUsedPercent: &secondary, SecondaryResetAt: 1_210_600, SecondaryWindowMinutes: 10080,
		PlanType: "pro",
	}
	if err = s.insertEvent(ctx, pro, time.Minute); err != nil {
		t.Fatal(err)
	}

	a := &app{cfg: defaultConfig(), store: s}
	check := func(account string, wantWeekly bool) {
		t.Helper()
		response := a.handleManagement(managementRequest{
			Method: "GET",
			Path:   "/cpa-quota-estimator/series",
			Query:  url.Values{"account": {account}},
		})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("account %s status=%d body=%s", account, response.StatusCode, response.Body)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(response.Body, &payload); err != nil {
			t.Fatal(err)
		}
		var detected bool
		if err := json.Unmarshal(payload["five_hour_quota_detected"], &detected); err != nil {
			t.Fatal(err)
		}
		_, hasWeekly := payload["weekly_quota"]
		if detected != wantWeekly || hasWeekly != wantWeekly {
			t.Fatalf("account %s detected=%v weekly=%v payload=%s", account, detected, hasWeekly, response.Body)
		}
	}
	check(plus.Account, true)
	check(pro.Account, false)
}

func TestManagementExposesBothSparkQuotaAxes(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "spark-dual-axis-api.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	now := time.Now().Unix()
	account := "spark-dual-axis"
	mainUsed := 40.0
	if err = s.insertEvent(ctx, event{
		RequestedAt: now - 900, ObservedAt: now - 900, Account: account,
		Provider: "openai", Model: "gpt-5.6-sol", TotalTokens: 100,
		UsedPercent: &mainUsed, ResetAt: now + 6*24*60*60, WindowMinutes: weeklyWindowMinutes, PlanType: "pro",
	}, time.Minute); err != nil {
		t.Fatal(err)
	}
	for index, percentages := range [][2]float64{{10, 20}, {15, 24}} {
		primary, weekly := percentages[0], percentages[1]
		at := now - 600 + int64(index)*300
		if err = s.insertEvent(ctx, event{
			RequestedAt: at, ObservedAt: at, Account: account,
			Provider: "openai", Model: "gpt-5.3-codex-spark", TotalTokens: int64(index+1) * 100,
			UsedPercent: &primary, ResetAt: now + 4*60*60, WindowMinutes: fiveHourWindowMinutes,
			SecondaryUsedPercent: &weekly, SecondaryResetAt: now + 6*24*60*60, SecondaryWindowMinutes: weeklyWindowMinutes,
			PlanType: "pro", QuotaScope: sparkQuotaScope,
		}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	a := &app{cfg: defaultConfig(), store: s}
	response := a.handleManagement(managementRequest{
		Method: "GET",
		Path:   "/cpa-quota-estimator/series",
		Query:  url.Values{"account": {account}, "include_spark": {"1"}},
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	var payload map[string]json.RawMessage
	if err = json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	var detected bool
	if err = json.Unmarshal(payload["spark_five_hour_quota_detected"], &detected); err != nil {
		t.Fatal(err)
	}
	if !detected {
		t.Fatalf("Spark dual axis was not detected: %s", response.Body)
	}
	var fiveHour, weekly scopedQuotaSeries
	if err = json.Unmarshal(payload["spark_quota"], &fiveHour); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(payload["spark_weekly_quota"], &weekly); err != nil {
		t.Fatal(err)
	}
	if fiveHour.WindowMinutes != fiveHourWindowMinutes || fiveHour.UsedPercent != 15 {
		t.Fatalf("Spark five-hour axis = %#v", fiveHour)
	}
	if weekly.WindowMinutes != weeklyWindowMinutes || weekly.UsedPercent != 24 {
		t.Fatalf("Spark weekly axis = %#v", weekly)
	}

	monthlyResponse := a.handleManagement(managementRequest{
		Method: "GET",
		Path:   "/cpa-quota-estimator/monthly",
		Query: url.Values{
			"account":       {account},
			"month":         {time.Now().In(shanghaiLocation()).Format("2006-01")},
			"include_spark": {"1"},
		},
	})
	if monthlyResponse.StatusCode != http.StatusOK {
		t.Fatalf("monthly status=%d body=%s", monthlyResponse.StatusCode, monthlyResponse.Body)
	}
	var monthlyPayload map[string]json.RawMessage
	if err = json.Unmarshal(monthlyResponse.Body, &monthlyPayload); err != nil {
		t.Fatal(err)
	}
	if _, ok := monthlyPayload["spark_summary"]; !ok {
		t.Fatalf("Spark five-hour monthly summary missing: %s", monthlyResponse.Body)
	}
	weeklyMonthlyRaw, ok := monthlyPayload["spark_weekly_summary"]
	if !ok {
		t.Fatalf("Spark weekly monthly summary missing: %s", monthlyResponse.Body)
	}
	var weeklyMonthly monthlySummary
	if err = json.Unmarshal(weeklyMonthlyRaw, &weeklyMonthly); err != nil {
		t.Fatal(err)
	}
	if weeklyMonthly.CycleCount != 1 || len(weeklyMonthly.Cycles) != 1 || weeklyMonthly.Cycles[0].WindowMinutes != weeklyWindowMinutes {
		t.Fatalf("Spark weekly monthly cycles = %#v", weeklyMonthly)
	}
}

func TestManagementExposesRecoveredQuotaRegimeAnomaly(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "quota-anomaly-api.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	const (
		account        = "quota-anomaly-api"
		originalReset  = int64(10_000)
		anomalousReset = int64(20_000)
		windowMinutes  = int64(100)
	)
	for _, item := range []struct {
		at      int64
		used    float64
		resetAt int64
	}{{100, 49, originalReset}, {200, 50, originalReset},
		{300, 70, anomalousReset}, {400, 71, anomalousReset},
		{500, 50, originalReset}, {600, 51, originalReset}} {
		used := item.used
		if err = s.insertEvent(ctx, event{
			RequestedAt: item.at, ObservedAt: item.at, Account: account,
			Provider: "openai", Model: "gpt", TotalTokens: 100,
			UsedPercent: &used, ResetAt: item.resetAt, WindowMinutes: windowMinutes, PlanType: "pro",
		}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	a := &app{cfg: defaultConfig(), store: s}
	for _, endpoint := range []string{"/cpa-quota-estimator/summary", "/cpa-quota-estimator/series"} {
		response := a.handleManagement(managementRequest{
			Method: "GET", Path: endpoint, Query: url.Values{"account": {account}},
		})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", endpoint, response.StatusCode, response.Body)
		}
		var payload struct {
			Latest         quotaPoint           `json:"latest"`
			Points         []quotaPoint         `json:"points"`
			CapacityPoints []capacityPoint      `json:"capacity_points"`
			Anomalies      []quotaRegimeAnomaly `json:"quota_anomalies"`
			RangeAnomalies []quotaRegimeAnomaly `json:"range_quota_anomalies"`
		}
		if err = json.Unmarshal(response.Body, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Anomalies) != 1 || payload.Anomalies[0].Kind != quotaRegimeReverted ||
			payload.Anomalies[0].PeakAt != 400 || payload.Anomalies[0].PeakUsedPercent != 71 {
			t.Fatalf("%s anomalies=%#v", endpoint, payload.Anomalies)
		}
		if endpoint == "/cpa-quota-estimator/summary" &&
			(payload.Latest.UsedPercent != 51 || payload.Latest.ResetAt != originalReset) {
			t.Fatalf("summary latest=%#v", payload.Latest)
		}
		if endpoint == "/cpa-quota-estimator/series" {
			if len(payload.Points) == 0 || payload.Points[len(payload.Points)-1].UsedPercent != 51 {
				t.Fatalf("series points=%#v", payload.Points)
			}
			if len(payload.RangeAnomalies) != 1 {
				t.Fatalf("series range anomalies=%#v", payload.RangeAnomalies)
			}
			var recoveryCapacity *capacityPoint
			for index := range payload.CapacityPoints {
				if payload.CapacityPoints[index].Time == 500 {
					recoveryCapacity = &payload.CapacityPoints[index]
					break
				}
			}
			if recoveryCapacity == nil || !recoveryCapacity.BreakBefore {
				t.Fatalf("series recovery capacity=%#v; points=%#v", recoveryCapacity, payload.CapacityPoints)
			}
		}
	}
}

func TestPricingSettingsManagementSwitchesPricingMode(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "pricing-mode-api.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}

	response := a.handleManagement(managementRequest{
		Method: "POST",
		Path:   "/cpa-quota-estimator/pricing-settings",
		Body:   []byte(`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"pricing_mode":"credits"}`),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	if a.cfg.PricingMode != pricingModeCredits {
		t.Fatalf("app pricing mode = %q", a.cfg.PricingMode)
	}
	var payload map[string]any
	if err = json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["pricing_mode"] != pricingModeCredits || payload["value_unit"] != "credits" {
		t.Fatalf("pricing response = %#v", payload)
	}

	response = a.handleManagement(managementRequest{
		Method: "POST",
		Path:   "/cpa-quota-estimator/pricing-settings",
		Body:   []byte(`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"pricing_mode":"legacy_api"}`),
	})
	if response.StatusCode != http.StatusOK || a.cfg.PricingMode != pricingModeLegacyAPI {
		t.Fatalf("switch back status=%d mode=%q body=%s", response.StatusCode, a.cfg.PricingMode, response.Body)
	}

	response = a.handleManagement(managementRequest{
		Method: "POST",
		Path:   "/cpa-quota-estimator/pricing-settings",
		Body:   []byte(`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"pricing_mode":"discounted"}`),
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid mode status=%d body=%s", response.StatusCode, response.Body)
	}
}
