package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSlowAttributionWriterDoesNotHoldAppLock(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "lock.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	entered := make(chan struct{})
	release := make(chan struct{})
	a := &app{store: s, cfg: defaultConfig()}
	a.attributionWriter = func(context.Context, *store, *weightFit, int64) (int64, error) {
		close(entered)
		<-release
		return 0, nil
	}
	fit := &weightFit{Available: true}
	done := make(chan struct{})
	go func() { a.applyFittedWeights(context.Background(), s, fit); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("attribution writer did not start")
	}
	lockAvailable := make(chan struct{})
	go func() { a.mu.RLock(); a.mu.RUnlock(); close(lockAvailable) }()
	select {
	case <-lockAvailable:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("app lock was held during slow attribution I/O")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("fit application did not finish")
	}
}

func TestIdenticalHostReconfigurationKeepsCapabilitiesDuringRepricing(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "reconfigure.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	raw := []byte("data_path: " + s.path + "\npricing_mode: credits\n")
	requested, err := parseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{store: s, cfg: requested, requestedConfig: requested, configured: true, pricingTask: &pricingRecalcTask{ID: "migration", Status: "running"}}
	// A saved runtime basis may differ from the host's unchanged config.
	a.cfg.PricingMode = pricingModeAPI
	for _, status := range []string{"queued", "running", "rolling_back"} {
		a.pricingTask.Status = status
		if err := a.configure(raw); err != nil {
			t.Fatalf("identical %s reconfigure failed: %v", status, err)
		}
		if a.store != s || a.cfg.PricingMode != pricingModeAPI || a.pricingTask.ID != "migration" {
			t.Fatal("idempotent reconfigure changed active state")
		}
	}
	if err := a.configure([]byte("data_path: " + s.path + "\npricing_mode: credits\nweight_fit_interval_minutes: 120\n")); err == nil {
		t.Fatal("a real config change should wait for repricing")
	}
}
