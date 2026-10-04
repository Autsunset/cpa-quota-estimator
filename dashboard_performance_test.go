package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Use a consistent online backup; never migrate or modify the source database.
func TestDashboardSnapshotPerformance(t *testing.T) {
	path := os.Getenv("DASHBOARD_SNAPSHOT")
	if path == "" {
		t.Skip("set DASHBOARD_SNAPSHOT to measure dashboard endpoints")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s := &store{db: db}
	cfg := defaultConfig()
	fit, ok, err := s.latestWeightFit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		cfg.LearnedFit = &fit.FittedWeights
	}
	a := &app{store: s, cfg: cfg}
	accounts, err := s.accounts(context.Background())
	if err != nil || len(accounts) == 0 {
		t.Fatalf("accounts unavailable: %v", err)
	}
	t.Logf("accounts=%d", len(accounts))
	for _, endpoint := range []string{"overview", "summary", "series", "monthly", "usage", "weights", "weights/backtest", "prices"} {
		start := time.Now()
		response := a.handleManagement(managementRequest{Method: "GET", Path: "/" + endpoint, Query: url.Values{"account": {accounts[0]}, "limit": {"5000"}}})
		t.Logf("%s: %s, %d bytes, status %d", endpoint, time.Since(start), len(response.Body), response.StatusCode)
		if response.StatusCode != 200 || !json.Valid(response.Body) {
			t.Fatalf("%s failed: %s", endpoint, response.Body)
		}
		if dir := os.Getenv("DASHBOARD_OUTPUT"); dir != "" {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, endpoint+".json")), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, endpoint+".json"), response.Body, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
