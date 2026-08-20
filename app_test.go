package main

import (
	"context"
	"net/http"
	"path/filepath"
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
