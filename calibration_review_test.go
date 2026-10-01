package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type reviewCase struct {
	Name     string          `json:"name"`
	Training int             `json:"training"`
	Scores   []backtestScore `json:"scores"`
	Trace    [][3]float64    `json:"trace"`
}

var reviewTrace [][3]float64

type reviewReport struct {
	Version  string       `json:"version"`
	Lag      int          `json:"lag"`
	Eligible int          `json:"eligible"`
	Train    int          `json:"train"`
	Tail     int          `json:"tail"`
	Cases    []reviewCase `json:"cases"`
	Seconds  float64      `json:"seconds"`
}

func reviewForecast(train, tail []quotaSegment, prices map[string]price) ([]backtestScore, error) {
	reviewTrace = nil
	opts := defaultWeightLearnerOptions()
	now := train[len(train)-1].EndAt
	fit, err := fitQuotaWeights(train, prices, now, opts)
	if err != nil {
		return nil, err
	}
	learned := newOnlineScaleTracker(fit, opts.RandomWalkSigma)
	ref, err := fitReferenceCycleScales(train, prices, pricingModeCredits, now, opts)
	if err != nil {
		return nil, err
	}
	official := newOnlineScaleTracker(ref, opts.RandomWalkSigma)
	a, b := scoreAccumulator{}, scoreAccumulator{}
	for _, s := range tail {
		w := segmentFitWeight(s, s.EndAt, opts.HalfLifeDays)
		eq := referenceEquivalent(s, pricingModeCredits, prices)
		rs := official.forSegment(s)
		referencePrediction := math.Exp(rs.LogValue) * eq
		a.add(referencePrediction, s.DP)
		rs.update(eq, s.DP, w, 0)
		if fit.Available {
			le := learnedSegmentEquivalent(s, fit)
			ls := learned.forSegment(s)
			b.add(math.Exp(ls.LogValue)*le, s.DP)
			reviewTrace = append(reviewTrace, [3]float64{s.DP, referencePrediction, math.Exp(ls.LogValue) * le})
			ls.update(le, s.DP, w, 0)
		}
	}
	return []backtestScore{a.result("credits"), b.result("weights_model")}, nil
}

func TestReviewerFrozenTail(t *testing.T) {
	started := time.Now()
	snapshot := os.Getenv("REVIEW_SNAPSHOT")
	if snapshot == "" {
		t.Skip("REVIEW_SNAPSHOT required")
	}
	// Evaluate an isolated copy; migrations and diagnostics must never alter the source snapshot.
	source, err := os.Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	working := filepath.Join(t.TempDir(), "review.sqlite")
	target, err := os.Create(working)
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
	s, err := openStore(working)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	prices := map[string]price{}
	ps, err := s.listPrices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		prices[normalizeModel(p.Model)] = p
	}
	byLag := map[int][]quotaSegment{}
	for lag := 0; lag < 3; lag++ {
		rows, err := s.quotaSegments(context.Background(), lag)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range rows {
			if q.Window == mainQuotaScope && q.eligible() && q.DP > 0 && referenceEquivalent(q, pricingModeCredits, prices) > 0 {
				byLag[lag] = append(byLag[lag], q)
			}
		}
		sortSegmentsChronologically(byLag[lag])
	}
	// Freeze lag=2 based on the pre-existing deployment's selected lag, before looking at the holdout.
	rows := byLag[2]
	if len(rows) < 15 {
		t.Skip("need at least 15 eligible main-quota segments")
	}
	n := len(rows) * 80 / 100
	train, tail := rows[:n], rows[n:]
	report := reviewReport{Version: pluginVersion, Lag: 2, Eligible: len(rows), Train: len(train), Tail: len(tail)}
	scores, err := reviewForecast(train, tail, prices)
	if err != nil {
		t.Fatal(err)
	}
	report.Cases = append(report.Cases, reviewCase{"frozen_last_20_percent", len(train), scores, reviewTrace})
	// Use recent fixed prefixes, then identical next 20 observations. Do not update model factors with tail targets.
	for _, size := range []int{10, 25, 50, 100} {
		if len(train) < size {
			continue
		}
		scores, err = reviewForecast(train[len(train)-size:], tail[:min(20, len(tail))], prices)
		if err != nil {
			t.Fatal(err)
		}
		report.Cases = append(report.Cases, reviewCase{"recent_small_training", size, scores, reviewTrace})
	}
	report.Seconds = time.Since(started).Seconds()
	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv("REVIEW_OUTPUT"); p != "" {
		if err = os.WriteFile(p, out, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Log(string(out))
}
