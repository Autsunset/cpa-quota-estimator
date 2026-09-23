package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type snapshotSigmaResult struct {
	Sigma  float64         `json:"sigma"`
	Scores []backtestScore `json:"scores"`
}

type snapshotAstraResult struct {
	Sigma                  float64                   `json:"sigma"`
	Segments               int                       `json:"segments"`
	AstraRelativeToCredits weightEstimate            `json:"astra_relative_to_credits"`
	AstraDiagnostic        identifiabilityDiagnostic `json:"astra_diagnostic"`
}

type snapshotHypothesisResult struct {
	AstraFactor   float64                `json:"astra_factor"`
	Objective     float64                `json:"objective"`
	Score         backtestScore          `json:"score"`
	ScaleSequence []hypothesisCycleScale `json:"scale_sequence"`
}

type hypothesisCycleScale struct {
	CycleID           int64   `json:"cycle_id"`
	CreditsPerPercent float64 `json:"credits_per_percent"`
}

type snapshotSensitivityResult struct {
	Sigma            []snapshotSigmaResult      `json:"sigma"`
	Astra            []snapshotAstraResult      `json:"astra"`
	StrictAstra      snapshotAstraResult        `json:"strict_astra"`
	Hypotheses       []snapshotHypothesisResult `json:"hypotheses"`
	StrictHypotheses []snapshotHypothesisResult `json:"strict_hypotheses"`
	Interrupted      interruptionSensitivity    `json:"interrupted"`
}

func TestSnapshotSensitivity(t *testing.T) {
	sourcePath := os.Getenv("CALIBRATION_SNAPSHOT")
	if sourcePath == "" {
		t.Skip("set CALIBRATION_SNAPSHOT")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(t.TempDir(), "sensitivity.sqlite")
	target, err := os.Create(path)
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
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	segments, err := s.quotaSegments(ctx, 2)
	if err != nil {
		t.Fatal(err)
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
	eligible := make([]quotaSegment, 0, len(segments))
	var restricted []quotaSegment
	var strict []quotaSegment
	targets := map[string]bool{"gpt-5.6-sol": true, "gpt-6-astra": true, "gpt-6-sol": true}
	for _, segment := range segments {
		if !segment.eligible() || segment.DP <= 0 || referenceEquivalent(segment, pricingModeCredits, prices) <= 0 {
			continue
		}
		eligible = append(eligible, segment)
		if segment.Window != mainQuotaScope || segment.CycleID < 7 || segment.CycleID > 11 {
			continue
		}
		var targetValue float64
		allTarget := true
		for _, feature := range segment.Features {
			if targets[normalizeModel(feature.Model)] {
				targetValue += float64(feature.Tokens) / 1_000_000 * referenceFeatureRate(feature, pricingModeCredits, prices) / referenceSolRate(pricingModeCredits, prices)
			}
			if !targets[normalizeModel(feature.Model)] {
				allTarget = false
			}
		}
		if targetValue/referenceEquivalent(segment, pricingModeCredits, prices) >= .95 {
			restricted = append(restricted, segment)
		}
		if allTarget {
			strict = append(strict, segment)
		}
	}
	sortSegmentsChronologically(eligible)
	sortSegmentsChronologically(restricted)
	sortSegmentsChronologically(strict)
	t.Logf("all eligible=%d restricted cycle7-11=%d strict=%d", len(eligible), len(restricted), len(strict))
	result := snapshotSensitivityResult{}
	strictFit, strictErr := fitQuotaWeights(strict, prices, now, defaultWeightLearnerOptions())
	if strictErr != nil {
		t.Fatal(strictErr)
	}
	result.StrictAstra = snapshotAstraResult{Sigma: .35, Segments: strictFit.SegmentCount}
	for _, row := range strictFit.Models {
		if row.Model == "gpt-6-astra" {
			result.StrictAstra.AstraRelativeToCredits = row.Input
			result.StrictAstra.AstraRelativeToCredits.Value /= 2.5
			result.StrictAstra.AstraRelativeToCredits.Low /= 2.5
			result.StrictAstra.AstraRelativeToCredits.High /= 2.5
		}
	}
	for _, diag := range strictFit.ModelDiagnostics {
		if diag.Name == "model:gpt-6-astra" {
			result.StrictAstra.AstraDiagnostic = diag
		}
	}
	for _, sigma := range []float64{.15, .35, .7} {
		opts := defaultWeightLearnerOptions()
		opts.RandomWalkSigma = sigma
		scores, scoreErr := prequentialScores(eligible, prices, opts)
		if scoreErr != nil {
			t.Fatal(scoreErr)
		}
		result.Sigma = append(result.Sigma, snapshotSigmaResult{Sigma: sigma, Scores: scores})
		fit, fitErr := fitQuotaWeights(restricted, prices, now, opts)
		if fitErr != nil {
			t.Fatal(fitErr)
		}
		astra := snapshotAstraResult{Sigma: sigma, Segments: fit.SegmentCount}
		for _, row := range fit.Models {
			if row.Model == "gpt-6-astra" {
				astra.AstraRelativeToCredits = row.Input
				astra.AstraRelativeToCredits.Value /= 2.5
				astra.AstraRelativeToCredits.Low /= 2.5
				astra.AstraRelativeToCredits.High /= 2.5
			}
		}
		for _, diag := range fit.ModelDiagnostics {
			if diag.Name == "model:gpt-6-astra" {
				astra.AstraDiagnostic = diag
			}
		}
		result.Astra = append(result.Astra, astra)
	}
	for _, factor := range []float64{1, 1.4} {
		opts := defaultWeightLearnerOptions()
		opts.FixedModelFactors = map[string]float64{"gpt-6-astra": factor}
		fit, fitErr := fitQuotaWeights(restricted, prices, now, opts)
		if fitErr != nil {
			t.Fatal(fitErr)
		}
		scores, scoreErr := prequentialScores(restricted, prices, opts)
		if scoreErr != nil {
			t.Fatal(scoreErr)
		}
		hypothesis := snapshotHypothesisResult{AstraFactor: factor, Objective: fit.Objective, Score: scores[len(scores)-1]}
		for _, cycle := range fit.CycleScales {
			if cycle.Window == mainQuotaScope && cycle.CycleID >= 7 && cycle.CycleID <= 11 {
				hypothesis.ScaleSequence = append(hypothesis.ScaleSequence, hypothesisCycleScale{CycleID: cycle.CycleID, CreditsPerPercent: cycle.CreditsPerPercent.Value})
			}
		}
		result.Hypotheses = append(result.Hypotheses, hypothesis)
		strictFit, strictFitErr := fitQuotaWeights(strict, prices, now, opts)
		if strictFitErr != nil {
			t.Fatal(strictFitErr)
		}
		strictScores, strictScoreErr := prequentialScores(strict, prices, opts)
		if strictScoreErr != nil {
			t.Fatal(strictScoreErr)
		}
		strictHypothesis := snapshotHypothesisResult{AstraFactor: factor, Objective: strictFit.Objective, Score: strictScores[len(strictScores)-1]}
		for _, cycle := range strictFit.CycleScales {
			if cycle.Window == mainQuotaScope && cycle.CycleID >= 7 && cycle.CycleID <= 11 {
				strictHypothesis.ScaleSequence = append(strictHypothesis.ScaleSequence, hypothesisCycleScale{CycleID: cycle.CycleID, CreditsPerPercent: cycle.CreditsPerPercent.Value})
			}
		}
		result.StrictHypotheses = append(result.StrictHypotheses, strictHypothesis)
	}
	result.Interrupted, err = runInterruptedSensitivity(segments, prices, now, defaultWeightLearnerOptions())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range result.Sigma {
		t.Logf("sigma=%g scores=%+v", row.Sigma, row.Scores)
	}
	for _, row := range result.Astra {
		t.Logf("astra sigma=%g n=%d ratio=%+v diag=%+v", row.Sigma, row.Segments, row.AstraRelativeToCredits, row.AstraDiagnostic)
	}
	t.Logf("strict astra=%+v", result.StrictAstra)
	for _, row := range result.Hypotheses {
		t.Logf("hypothesis astra=%g objective=%g score=%+v scale=%+v", row.AstraFactor, row.Objective, row.Score, row.ScaleSequence)
	}
	for _, row := range result.StrictHypotheses {
		t.Logf("strict hypothesis astra=%g objective=%g score=%+v scale=%+v", row.AstraFactor, row.Objective, row.Score, row.ScaleSequence)
	}
	t.Logf("interrupts=%+v", result.Interrupted)
	if output := os.Getenv("CALIBRATION_SENSITIVITY_OUTPUT"); output != "" {
		raw, errJSON := json.MarshalIndent(result, "", "  ")
		if errJSON != nil {
			t.Fatal(errJSON)
		}
		if err = os.WriteFile(output, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
