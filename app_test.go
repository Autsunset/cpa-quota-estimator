package main

import (
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

func TestManagementRegistrationIncludesAllHandledRoutes(t *testing.T) {
	registration := managementRegistration().(map[string]any)
	routes := registration["routes"].([]map[string]any)
	registered := make(map[string]bool, len(routes))
	for _, route := range routes {
		registered[route["Method"].(string)+" "+route["Path"].(string)] = true
	}

	for _, expected := range []string{
		"GET /cpa-quota-estimator/summary",
		"GET /cpa-quota-estimator/series",
		"GET /cpa-quota-estimator/monthly",
		"GET /cpa-quota-estimator/repair/early-resets",
		"POST /cpa-quota-estimator/repair/early-resets",
		"GET /cpa-quota-estimator/prices",
		"POST /cpa-quota-estimator/prices/sync",
		"GET /cpa-quota-estimator/pricing-settings",
		"POST /cpa-quota-estimator/pricing-settings",
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
