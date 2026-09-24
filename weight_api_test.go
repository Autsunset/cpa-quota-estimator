package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestWeightRoutesRemainAvailableAfterLegacyModeMigration(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "weights-api.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := app{cfg: defaultConfig(), store: s}
	request := managementRequest{Method: "POST", Path: "/cpa-quota-estimator/pricing-settings",
		Body: []byte(`{"pricing_mode":"learned"}`)}
	response := a.handleManagement(request)
	if response.StatusCode != 202 {
		t.Fatalf("legacy learned alias status=%d", response.StatusCode)
	}
	var task pricingRecalcTask
	if err = json.Unmarshal(response.Body, &task); err != nil {
		t.Fatal(err)
	}
	waitPricingTask(t, &a, task.ID)
	if a.cfg.PricingMode != pricingModeCredits {
		t.Fatalf("migrated mode=%s", a.cfg.PricingMode)
	}
	fit := weightFit{Available: true, Lag: 1, SegmentCount: 25, Models: []learnedModelWeights{{Model: "gpt-5.6-sol", Input: weightEstimate{Value: 1}}}}
	backtest := weightBacktest{SelectedLag: 1, FittedWeights: fit, Scores: []backtestScore{{Mode: "weights_model", MAE: .2, SegmentCount: 10}}}
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
	if pricingValueUnit(a.cfg.PricingMode) != "credits" {
		t.Fatal("legacy learned alias must use Credits")
	}
}
