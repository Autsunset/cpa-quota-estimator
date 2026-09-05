package main

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
)

func TestCalibrationSettingsAPIAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "calibration.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = s.upsertPrices(ctx, []price{astraTestPrice(), {Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: .4}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,requested_at,account,model,input_tokens,cost_usd) VALUES(1,100,'a','gpt-6-astra',1000000,18),(1,100,'a','gpt-5.6-sol',1000000,4);
 INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,window_cost_usd) VALUES(1,100,'a',1,1000,15,22);`); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: defaultConfig(), store: s}
	cases := []struct {
		body       string
		want, mult float64
		enabled    bool
	}{
		{`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"apply_model_calibration":true,"astra_multiplier":2.3}`, 23, 2.3, true},
		{`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"apply_model_calibration":false}`, 10, 2.3, false},
		// Old clients and pricing-mode changes must not re-enable calibration.
		{`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"pricing_mode":"legacy_api"}`, 10, 2.3, false},
		{`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"apply_model_calibration":true}`, 23, 2.3, true},
		{`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"astra_multiplier":0.5}`, 5, .5, true},
		{`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"astra_multiplier":1.8,"pricing_mode":"current_api"}`, 18, 1.8, true},
	}
	for _, tc := range cases {
		resp := a.handleManagement(managementRequest{Method: "POST", Path: "/cpa-quota-estimator/pricing-settings", Body: []byte(tc.body)})
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
		}
		var returned map[string]any
		if err = json.Unmarshal(resp.Body, &returned); err != nil {
			t.Fatal(err)
		}
		if returned["apply_model_calibration"] != tc.enabled || returned["astra_multiplier"] != tc.mult {
			t.Fatalf("response=%s", resp.Body)
		}
		var cost float64
		if err = s.db.QueryRow(`SELECT cost_usd FROM usage_events WHERE model='gpt-6-astra'`).Scan(&cost); err != nil {
			t.Fatal(err)
		}
		if math.Abs(cost-tc.want) > 1e-8 {
			t.Fatalf("cost=%f want=%f", cost, tc.want)
		}
		var sample, sum float64
		if err = s.db.QueryRow(`SELECT window_cost_usd FROM quota_samples`).Scan(&sample); err != nil {
			t.Fatal(err)
		}
		if err = s.db.QueryRow(`SELECT SUM(cost_usd) FROM usage_events`).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		if math.Abs(sample-sum) > 1e-8 {
			t.Fatalf("sample=%f sum=%f", sample, sum)
		}
		saved, err := s.loadPricingSettings(ctx, defaultConfig().pricingSettings())
		if err != nil {
			t.Fatal(err)
		}
		restored := defaultConfig().withPricingSettings(saved)
		if restored.ApplyModelCalibration != tc.enabled || restored.AstraMultiplier != tc.mult {
			t.Fatalf("saved=%#v", saved)
		}
		rows, err := s.remainingModelAllowances(ctx, tc.want, restored)
		if err != nil {
			t.Fatal(err)
		}
		if rows[0].InputRate != 10 || math.Abs(rows[0].InputTokens-1e6) > 1e-7 {
			t.Fatalf("allowance=%#v", rows[0])
		}
	}
	for _, bad := range []string{"0", "-1", "101", "0.001", "1e999", "\"bad\""} {
		resp := a.handleManagement(managementRequest{Method: "POST", Path: "/cpa-quota-estimator/pricing-settings", Body: []byte(`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"astra_multiplier":` + bad + `}`)})
		if resp.StatusCode != 400 {
			t.Fatalf("bad=%s status=%d", bad, resp.StatusCode)
		}
		if a.cfg.AstraMultiplier != 1.8 {
			t.Fatal("invalid request changed config")
		}
	}
}

func TestOldSettingsDefaultCalibrationAndConfigValidation(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "old-settings.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	_, err = s.db.Exec(`INSERT INTO metadata(key,value) VALUES('pricing_settings','{"apply_long_context_pricing":false,"apply_fast_pricing":true,"pricing_mode":"legacy_api"}')`)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.loadPricingSettings(context.Background(), defaultConfig().pricingSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !settings.ApplyModelCalibration || settings.AstraMultiplier != 1.8 {
		t.Fatalf("old settings=%#v", settings)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), 0, -1, 101} {
		if validateAstraMultiplier(v) == nil {
			t.Fatalf("accepted %v", v)
		}
	}
	cfg, err := parseConfig([]byte("apply_model_calibration: false\nastra_multiplier: 2.4\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.modelPriceMultiplier("gpt-6-astra") != 1 || cfg.AstraMultiplier != 2.4 {
		t.Fatalf("cfg=%#v", cfg)
	}
}
