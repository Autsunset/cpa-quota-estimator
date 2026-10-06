package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"math/rand"
	"net/url"
	"os"
	"testing"
	"time"
)

func referenceSyntheticSegments(stable bool) []quotaSegment {
	const now = int64(1800000000)
	random := rand.New(rand.NewSource(20261007))
	segments := make([]quotaSegment, 300)
	for i := range segments {
		input := int64(300000 + random.Intn(2000000))
		cache := int64(1000000 + random.Intn(20000000))
		output := int64(20000 + random.Intn(600000))
		if stable {
			multiple := int64(1 + i%5)
			input, cache, output = 100000*multiple, 1000000*multiple, 20000*multiple
		}
		segments[i] = quotaSegment{Account: "synthetic", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: now + 86400,
			EndAt: now - int64(len(segments)-i)*30, BoundaryWeight: 1,
			Features: []segmentFeature{{Model: weightReferenceModel, Type: "input", Tokens: input},
				{Model: weightReferenceModel, Type: "cache", Tokens: cache}, {Model: weightReferenceModel, Type: "output", Tokens: output}},
			DP: .7 * (float64(input) + .16*float64(cache) + 7*float64(output)) / 1_000_000}
	}
	return segments
}

func TestReferenceModelLearnsCacheAndOutputWithoutMovingInputGauge(t *testing.T) {
	for _, stable := range []bool{false, true} {
		fit, err := fitQuotaWeights(referenceSyntheticSegments(stable), nil, 1800000000, defaultWeightLearnerOptions())
		if err != nil || !fit.Available {
			t.Fatalf("fit available=%t err=%v", fit.Available, err)
		}
		for _, name := range fit.ParameterNames {
			if name == componentParameter(weightReferenceModel, "input") || name == pooledParameter(weightReferenceModel) {
				t.Fatalf("reference input gauge became a free parameter: %s", name)
			}
		}
		var found bool
		for _, row := range fit.Models {
			if row.Model != weightReferenceModel {
				continue
			}
			found = true
			if row.Input.Source != "anchor" || row.Input.Value != 1 || row.Input.Low != 1 || row.Input.High != 1 || row.Input.Identified {
				t.Fatalf("reference input is a unit convention, not measured: %#v", row.Input)
			}
			for _, check := range []struct {
				estimate weightEstimate
				want     float64
			}{{row.Cache, .16}, {row.Output, 7}} {
				if stable {
					if !check.estimate.PriorLocked || check.estimate.Identified {
						t.Fatalf("stable reference composition falsely calibrated: %#v", check.estimate)
					}
				} else if !supportedWeight(check.estimate) || math.Abs(check.estimate.Value/check.want-1) > .08 {
					t.Fatalf("reference component=%#v want=%g", check.estimate, check.want)
				}
			}
		}
		if !found {
			t.Fatal("missing reference model")
		}
	}
}

func TestFormerReferenceInputUsesCalibratedInverseAnchor(t *testing.T) {
	cfg := defaultConfig()
	cfg.LearnedFit = testAnchorFit()
	cfg.LearnedFit.Models[0].Input = weightEstimate{Source: "anchor", Value: 1, Low: 1, High: 1, PriorLocked: true}
	cfg.LearnedFit.ObservedModels = []string{weightReferenceModel}
	p, _ := officialGPT6Price(weightReferenceModel)
	for _, mode := range []string{pricingModeAPI, pricingModeCredits} {
		cfg.PricingMode = mode
		cfg.AnchorModel = "gpt-6-astra"
		row := cfg.priceRow(p)
		adj := row.ComponentAdjustments["input"]
		if !adj.Calibrated || adj.Baseline || adj.Source != "independent" || !row.Observed ||
			math.Abs(adj.Factor-1/1.2) > 1e-9 || math.Abs(adj.Low-1/1.44) > 1e-9 || math.Abs(adj.High-1/.96) > 1e-9 {
			t.Fatalf("inverse anchor=%#v observed=%t", adj, row.Observed)
		}
		official := priceForPricingMode(p, mode)
		if math.Abs(row.Calculated.Input/official.Input-1/1.2) > 1e-9 {
			t.Fatalf("former reference input=%g official=%g", row.Calculated.Input, official.Input)
		}
		cfg.AnchorModel = weightReferenceModel
		if adj := cfg.priceRow(p).ComponentAdjustments["input"]; !adj.Baseline || adj.Calibrated || adj.Factor != 1 {
			t.Fatalf("restored baseline=%#v", adj)
		}
		cfg.AnchorModel = "gpt-6-sol"
		if adj := cfg.priceRow(p).ComponentAdjustments["input"]; adj.Calibrated {
			t.Fatalf("unidentified anchor falsely calibrates reference: %#v", adj)
		}
	}
}

func TestReferenceCacheOnlyEvidenceLeavesOutputAtPrior(t *testing.T) {
	segments := referenceSyntheticSegments(false)
	for i := range segments {
		segments[i].Features = segments[i].Features[:2]
		segments[i].DP = .7 * (float64(segments[i].Features[0].Tokens) + .16*float64(segments[i].Features[1].Tokens)) / 1_000_000
	}
	fit, err := fitQuotaWeights(segments, nil, 1800000000, defaultWeightLearnerOptions())
	if err != nil || !fit.Available {
		t.Fatalf("fit available=%t err=%v", fit.Available, err)
	}
	for _, row := range fit.Models {
		if row.Model == weightReferenceModel {
			if !supportedWeight(row.Cache) || math.Abs(row.Cache.Value/.16-1) > .08 || !row.Output.PriorLocked || row.Output.Identified || row.Input.Source != "anchor" {
				t.Fatalf("reference cache evidence leaked into input/output: %+v", row)
			}
			return
		}
	}
	t.Fatal("missing reference model")
}

func TestReferenceCalibrationSnapshot(t *testing.T) {
	path := os.Getenv("REFERENCE_SNAPSHOT")
	if path == "" {
		t.Skip("set REFERENCE_SNAPSHOT to fit a read-only online backup")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s := &store{db: db}
	ctx := context.Background()
	previous, ok, err := s.latestWeightFit(ctx)
	if err != nil || !ok {
		t.Fatalf("previous fit available=%t err=%v", ok, err)
	}
	segments, err := s.quotaSegments(ctx, previous.SelectedLag)
	if err != nil {
		t.Fatal(err)
	}
	prices, err := loadPriceCatalog(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	fit, err := fitQuotaWeights(segments, prices, time.Now().Unix(), defaultWeightLearnerOptions())
	if err != nil || !fit.Available {
		t.Fatalf("snapshot fit available=%t err=%v", fit.Available, err)
	}
	for _, row := range fit.Models {
		if row.Model == weightReferenceModel {
			raw, _ := json.Marshal(row)
			t.Logf("reference=%s", raw)
		}
	}
	for _, d := range fit.ComponentDiagnostics {
		if d.Model == weightReferenceModel {
			t.Logf("reference diagnostic=%+v", d)
		}
	}
	t.Logf("segments=%d previous_mae=%g current_mae=%g", fit.SegmentCount, previous.FittedWeights.MeanAbsError, fit.MeanAbsError)
	if path := os.Getenv("REFERENCE_OUTPUT"); path != "" {
		raw, err := json.Marshal(fit)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSwitchingAnchorPreservesReferenceSamplesAndFit(t *testing.T) {
	s, a := pricingFixture(t)
	defer s.close()
	a.cfg.LearnedFit = testAnchorFit()
	a.cfg.LearnedFit.Models[0].Input = weightEstimate{Source: "anchor", Value: 1, Low: 1, High: 1, PriorLocked: true}
	a.cfg.LearnedFit.ObservedModels = []string{weightReferenceModel}
	astra, _ := officialGPT6Price("gpt-6-astra")
	a.cfg.PriceCatalog[astra.Model] = astra
	original := a.cfg.LearnedFit
	for _, anchor := range []string{"gpt-6-astra", weightReferenceModel, "gpt-6-astra"} {
		body, _ := json.Marshal(map[string]string{"anchor_model": anchor, "pricing_mode": pricingModeCredits})
		response := a.handleManagement(managementRequest{Method: "POST", Path: "/pricing-settings", Body: body})
		if response.StatusCode != 202 {
			t.Fatalf("switch to %s: %d %s", anchor, response.StatusCode, response.Body)
		}
		var task pricingRecalcTask
		if err := json.Unmarshal(response.Body, &task); err != nil {
			t.Fatal(err)
		}
		if task := waitPricingTask(t, a, task.ID); task.Status != "succeeded" {
			t.Fatalf("anchor switch task=%+v", task)
		}
		var events, samples int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE model=?`, weightReferenceModel).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples`).Scan(&samples); err != nil {
			t.Fatal(err)
		}
		if events != 6 || samples != 8 || a.cfg.LearnedFit != original {
			t.Fatalf("anchor switch lost evidence: events=%d samples=%d retained_fit=%t", events, samples, a.cfg.LearnedFit == original)
		}
		p := a.cfg.PriceCatalog[weightReferenceModel]
		row := a.cfg.priceRow(p)
		if !row.Observed || (anchor != weightReferenceModel && !row.Adjustment.Calibrated) {
			t.Fatalf("former reference loses sample/calibration status: %+v", row)
		}
	}
}
