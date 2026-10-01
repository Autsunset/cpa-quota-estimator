package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The fixed development origins stay within the original 577-observation
// training prefix. A source snapshot is copied before schema initialization.
func TestReviewerEarlyDevelopment(t *testing.T) {
	sourcePath := os.Getenv("REVIEW_SNAPSHOT")
	if sourcePath == "" {
		t.Skip("set REVIEW_SNAPSHOT for the offline development comparison")
	}
	started := time.Now()
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	working := filepath.Join(t.TempDir(), "development.sqlite")
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
	rows, err := s.quotaSegments(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := s.listPrices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	prices := map[string]price{}
	for _, p := range ps {
		prices[normalizeModel(p.Model)] = p
	}
	eligible := []quotaSegment{}
	for _, q := range rows {
		if q.Window == mainQuotaScope && q.eligible() && q.DP > 0 && referenceEquivalent(q, pricingModeCredits, prices) > 0 {
			eligible = append(eligible, q)
		}
	}
	sortSegmentsChronologically(eligible)
	if len(eligible) < 520 {
		t.Skip("fixed development origins require at least 520 eligible primary-quota crossings")
	}
	output := []map[string]interface{}{}
	for _, origin := range []int{200, 300, 400, 500} {
		for _, n := range []int{10, 25, 50, 100} {
			scores, err := reviewForecast(eligible[origin-n:origin], eligible[origin:origin+20], prices)
			if err != nil {
				t.Fatal(err)
			}
			output = append(output, map[string]interface{}{"origin": origin, "n": n, "scores": scores})
		}
	}
	raw, err := json.MarshalIndent(map[string]interface{}{"version": pluginVersion, "seconds": time.Since(started).Seconds(), "cases": output}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("REVIEW_DEVELOPMENT_OUTPUT"); path != "" {
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Log(string(raw))
}
