package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestWeightRoutesAndLearnedPricingGuard(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "weights-api.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := app{cfg: defaultConfig(), store: s}
	request := managementRequest{Method: "POST", Path: "/cpa-quota-estimator/pricing-settings",
		Body: []byte(`{"apply_long_context_pricing":false,"apply_fast_pricing":true,"pricing_mode":"learned"}`)}
	if response := a.handleManagement(request); response.StatusCode != 400 {
		t.Fatalf("unfitted learned mode returned %d", response.StatusCode)
	}
	fit := weightFit{Available: true, Lag: 1, SegmentCount: 25, Models: []learnedModelWeights{{Model: "gpt-5.6-sol", Input: weightEstimate{Value: 1}}}}
	backtest := weightBacktest{SelectedLag: 1, FittedWeights: fit, Scores: []backtestScore{{Mode: pricingModeLearned, MAE: .2, SegmentCount: 10}}}
	rawFit, _ := json.Marshal(fit)
	rawBacktest, _ := json.Marshal(backtest)
	if _, err = s.db.Exec(`INSERT INTO weight_fits(fitted_at,lag,segment_count,fit_json,backtest_json) VALUES(1,1,25,?,?)`, string(rawFit), string(rawBacktest)); err != nil {
		t.Fatal(err)
	}
	a.cfg.LearnedFit = &fit
	for _, path := range []string{"/cpa-quota-estimator/weights", "/cpa-quota-estimator/weights/backtest"} {
		response := a.handleManagement(managementRequest{Method: "GET", Path: path})
		if response.StatusCode != 200 {
			t.Fatalf("%s returned %d", path, response.StatusCode)
		}
	}
	if response := a.handleManagement(request); response.StatusCode != 200 || a.cfg.PricingMode != pricingModeLearned {
		t.Fatalf("learned setting failed: status=%d mode=%s", response.StatusCode, a.cfg.PricingMode)
	}
	if pricingValueUnit(a.cfg.PricingMode) != "sol_input_equiv" {
		t.Fatal("wrong learned unit")
	}
}
