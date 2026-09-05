package main

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestOverviewReturnsEveryRecordedAccount(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "overview.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	now := time.Now().Unix()
	resetAt := ((now + 5*24*60*60 + 30) / 60) * 60
	seedOverviewAccount(t, s, "team-account", "team", now-3600, resetAt, []float64{10, 14})
	seedOverviewAccount(t, s, "k12-account", "k12", now-3000, resetAt, []float64{95, 100})

	a := &app{cfg: defaultConfig(), store: s}
	response := a.handleManagement(managementRequest{
		Method: "GET",
		Path:   "/cpa-quota-estimator/overview",
		Query:  url.Values{},
	})
	if response.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	var overview overviewResponse
	if err = json.Unmarshal(response.Body, &overview); err != nil {
		t.Fatal(err)
	}
	if overview.PluginVersion != pluginVersion {
		t.Fatalf("plugin_version=%q want %q", overview.PluginVersion, pluginVersion)
	}
	if overview.PricingMode != pricingModeLegacyAPI || overview.ValueUnit != "USD" {
		t.Fatalf("pricing_mode=%q value_unit=%q", overview.PricingMode, overview.ValueUnit)
	}
	if len(overview.Accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(overview.Accounts))
	}
	byAccount := make(map[string]accountOverview, len(overview.Accounts))
	for _, item := range overview.Accounts {
		byAccount[item.Account] = item
	}
	for account, plan := range map[string]string{"team-account": "team", "k12-account": "k12"} {
		item, ok := byAccount[account]
		if !ok {
			t.Fatalf("missing account %q", account)
		}
		if item.PlanType != plan || item.Latest == nil {
			t.Fatalf("account=%q plan=%q latest=%v", account, item.PlanType, item.Latest)
		}
		if item.Latest.Requests != 2 || item.Latest.WindowTokens != 3000 {
			t.Fatalf("account=%q requests=%d tokens=%d", account, item.Latest.Requests, item.Latest.WindowTokens)
		}
		if !item.Estimate.Available || !item.BurnForecast.Available {
			t.Fatalf("account=%q estimate=%+v burn=%+v", account, item.Estimate, item.BurnForecast)
		}
	}
	if got := byAccount["team-account"]; got.RemainingPercent != 86 || got.QuotaStatus != "active" {
		t.Fatalf("team snapshot remaining=%v status=%q", got.RemainingPercent, got.QuotaStatus)
	}
	if got := byAccount["k12-account"]; got.RemainingPercent != 0 || got.QuotaStatus != "exhausted" {
		t.Fatalf("k12 snapshot remaining=%v status=%q", got.RemainingPercent, got.QuotaStatus)
	}
}

func TestOverviewIncludesIndependentWeeklyQuota(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "weekly-overview.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	now := time.Now().Unix()
	primaryReset := now + 4*60*60
	weeklyReset := now + 6*24*60*60
	for index, percentages := range [][2]float64{{10, 30}, {14, 35}} {
		primary, weekly := percentages[0], percentages[1]
		at := now - 1800 + int64(index)*600
		if err = s.insertEvent(context.Background(), event{
			RequestedAt:            at,
			ObservedAt:             at,
			Account:                "plus-account",
			Provider:               "openai",
			Model:                  "gpt-test",
			TotalTokens:            int64(index+1) * 1000,
			CostUSD:                float64(index + 1),
			UsedPercent:            &primary,
			ResetAt:                primaryReset,
			WindowMinutes:          fiveHourWindowMinutes,
			SecondaryUsedPercent:   &weekly,
			SecondaryResetAt:       weeklyReset,
			SecondaryWindowMinutes: weeklyWindowMinutes,
			PlanType:               "plus",
		}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	a := &app{cfg: defaultConfig(), store: s}
	response := a.handleManagement(managementRequest{Method: "GET", Path: "/cpa-quota-estimator/overview", Query: url.Values{}})
	if response.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	var overview overviewResponse
	if err = json.Unmarshal(response.Body, &overview); err != nil {
		t.Fatal(err)
	}
	if len(overview.Accounts) != 1 {
		t.Fatalf("accounts=%d want 1", len(overview.Accounts))
	}
	item := overview.Accounts[0]
	if !item.FiveHourQuotaDetected || item.WeeklyQuota == nil {
		t.Fatalf("weekly quota missing: %+v", item)
	}
	if item.RemainingPercent != 86 || item.QuotaStatus != "active" {
		t.Fatalf("primary remaining=%v status=%q", item.RemainingPercent, item.QuotaStatus)
	}
	weekly := item.WeeklyQuota
	if weekly.RemainingPercent != 65 || weekly.QuotaStatus != "active" || weekly.Latest == nil {
		t.Fatalf("weekly overview=%+v", weekly)
	}
	if weekly.Latest.Requests != 2 || weekly.Latest.WindowTokens != 3000 {
		t.Fatalf("weekly latest=%+v", weekly.Latest)
	}
	if !weekly.Estimate.Available || !weekly.BurnForecast.Available {
		t.Fatalf("weekly estimate=%+v burn=%+v", weekly.Estimate, weekly.BurnForecast)
	}
}

func TestQuotaSnapshotState(t *testing.T) {
	now := time.Now().Unix()
	for _, test := range []struct {
		name      string
		point     quotaPoint
		remaining float64
		status    string
	}{
		{name: "active", point: quotaPoint{UsedPercent: 20, ResetAt: now + 60}, remaining: 80, status: "active"},
		{name: "exhausted", point: quotaPoint{UsedPercent: 100, ResetAt: now + 60}, remaining: 0, status: "exhausted"},
		{name: "stale after reset", point: quotaPoint{UsedPercent: 75, ResetAt: now - 1}, remaining: 25, status: "awaiting_refresh"},
		{name: "clamps invalid percent", point: quotaPoint{UsedPercent: 120, ResetAt: now + 60}, remaining: 0, status: "exhausted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			remaining, status := quotaSnapshotState(test.point, now)
			if remaining != test.remaining || status != test.status {
				t.Fatalf("remaining=%v status=%q want remaining=%v status=%q", remaining, status, test.remaining, test.status)
			}
		})
	}
}

func TestOverviewReturnsEmptyArrayWithoutSamples(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "empty-overview.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}
	response := a.handleManagement(managementRequest{Method: "GET", Path: "/cpa-quota-estimator/overview", Query: url.Values{}})
	if response.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(response.Body, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["accounts"]) != "[]" {
		t.Fatalf("accounts=%s want []", raw["accounts"])
	}
}

func seedOverviewAccount(t *testing.T, s *store, account, plan string, startedAt, resetAt int64, usedValues []float64) {
	t.Helper()
	for index, used := range usedValues {
		usedCopy := used
		at := startedAt + int64(index)*600
		err := s.insertEvent(context.Background(), event{
			RequestedAt:   at,
			ObservedAt:    at,
			Account:       account,
			Provider:      "openai",
			Model:         "gpt-test",
			TotalTokens:   int64(index+1) * 1000,
			CostUSD:       float64(index + 1),
			UsedPercent:   &usedCopy,
			ResetAt:       resetAt,
			WindowMinutes: 10080,
			PlanType:      plan,
		}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
	}
}
