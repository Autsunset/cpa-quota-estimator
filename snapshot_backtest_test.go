package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Run with CALIBRATION_SNAPSHOT=/path/to/read-only-copy and
// CALIBRATION_OUTPUT=/tmp/result.json. The source is copied before migration.
func TestSnapshotRollingBacktest(t *testing.T) {
	sourcePath := os.Getenv("CALIBRATION_SNAPSHOT")
	if sourcePath == "" {
		t.Skip("set CALIBRATION_SNAPSHOT to run the offline backtest")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	workingPath := filepath.Join(t.TempDir(), "working.sqlite")
	target, err := os.Create(workingPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(target, source); err != nil {
		target.Close()
		t.Fatal(err)
	}
	if err = target.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(workingPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	byLag := make(map[int][]quotaSegment)
	for lag := 0; lag <= 2; lag++ {
		byLag[lag], err = s.quotaSegments(ctx, lag)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("lag=%d segments=%d", lag, len(byLag[lag]))
	}
	priceRows, err := s.listPrices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prices := make(map[string]price)
	for _, p := range priceRows {
		prices[normalizeModel(p.Model)] = p
	}
	var now int64
	if err = s.db.QueryRow(`SELECT COALESCE(MAX(requested_at),0) FROM usage_events`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	result, err := rollingBacktest(byLag, prices, now, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, lag := range result.Lags {
		t.Logf("lag=%d eligible=%d flagged=%d scores=%+v", lag.Lag, lag.EligibleSegments, lag.FlaggedSegments, lag.Scores)
	}
	t.Logf("selected=%d ambiguous=%t fit=%d", result.SelectedLag, result.LagAmbiguous, result.FittedWeights.SegmentCount)
	saved, err := s.refreshWeightFit(ctx, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	count, err := s.updateWeightAttributions(ctx, &saved.FittedWeights, 272000)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("attributed history processed=%d", count)
	var attributed int64
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE learned_quota_pct IS NOT NULL`).Scan(&attributed); err != nil {
		t.Fatal(err)
	}
	if attributed == 0 {
		t.Fatal("offline fit attributed no requests")
	}
	cfg := defaultConfig()
	cfg.LearnedFit = &saved.FittedWeights
	cfg.PricingMode = pricingModeCredits
	if _, err = s.savePricingSettingsAndRecalculate(ctx, cfg.pricingSettings(), cfg); err != nil {
		t.Fatal(err)
	}
	var nonzero int64
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE model='gpt-6-astra' AND cost_usd>0`).Scan(&nonzero); err != nil {
		t.Fatal(err)
	}
	if nonzero == 0 {
		t.Fatal("learned pricing produced no Astra values")
	}
	a := app{store: s, cfg: cfg}
	for _, path := range []string{"/cpa-quota-estimator/weights", "/cpa-quota-estimator/weights/backtest"} {
		response := a.handleManagement(managementRequest{Method: "GET", Path: path})
		if response.StatusCode != 200 {
			t.Fatalf("%s returned %d", path, response.StatusCode)
		}
	}
	if outputPath := os.Getenv("CALIBRATION_OUTPUT"); outputPath != "" {
		raw, errJSON := json.MarshalIndent(result, "", "  ")
		if errJSON != nil {
			t.Fatal(errJSON)
		}
		if err = os.WriteFile(outputPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
