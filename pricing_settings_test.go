package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func pricingFixture(t *testing.T) (*store, *app) {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "pricing.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.upsertPrices(context.Background(), []price{{Model: "gpt-5.6-sol", Input: 4, CacheRead: .4, Output: 20, CacheWrite: 5, LongInput: 8, LongRead: .8, LongOutput: 30, LongWrite: 10}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes) VALUES(1,'a',100,1000,15),(2,'a',1100,2000,15)`); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		cycle, at int64
		scope     string
		tokens    int64
	}{{1, 300, "main", 1_000_000}, {1, 100, "main", 2_000_000}, {1, 200, "spark", 3_000_000}, {1, 300, "main", 4_000_000}, {2, 1200, "main", 5_000_000}, {2, 1300, "main", 6_000_000}} {
		if _, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,requested_at,account,model,input_tokens,total_tokens,cost_usd,quota_scope) VALUES(?,?,'a','gpt-5.6-sol',?, ?,99,?)`, item.cycle, item.at, item.tokens, item.tokens, item.scope); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ cycle, at int64 }{{1, 50}, {1, 100}, {1, 250}, {1, 300}, {1, 300}, {2, 1200}, {2, 1250}, {2, 1300}} {
		if _, err = s.db.Exec(`INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,window_cost_usd) VALUES(?,?,'a',10,2000,15,99)`, item.cycle, item.at); err != nil {
			t.Fatal(err)
		}
	}
	cfg := defaultConfig()
	cfg.PriceCatalog = map[string]price{"gpt-5.6-sol": {Model: "gpt-5.6-sol", Input: 4, CacheRead: .4, Output: 20, CacheWrite: 5}}
	return s, &app{cfg: cfg, store: s}
}

func waitPricingTask(t *testing.T, a *app, id string) pricingRecalcTask {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		task, ok := a.latestPricingTask(id)
		if ok && (task.Status == "succeeded" || task.Status == "failed") {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pricing task %s did not finish", id)
	return pricingRecalcTask{}
}

func TestPrefixSampleCostsMatchOldSQLForEverySample(t *testing.T) {
	s, a := pricingFixture(t)
	defer s.close()
	settings := a.cfg.pricingSettings()
	settings.PricingMode = pricingModeAPI
	count, err := s.recalculatePricing(context.Background(), settings, a.cfg, nil)
	if err != nil || count != 6 {
		t.Fatalf("recalculate count=%d err=%v", count, err)
	}
	rows, err := s.db.Query(`SELECT id,cycle_id,sampled_at,window_cost_usd FROM quota_samples ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type sample struct {
		id, cycle, at int64
		value         float64
	}
	var samples []sample
	for rows.Next() {
		var x sample
		if err = rows.Scan(&x.id, &x.cycle, &x.at, &x.value); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		samples = append(samples, x)
	}
	rows.Close()
	for _, sample := range samples {
		var oldSQL float64
		err = s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM usage_events WHERE cycle_id=? AND quota_scope='main' AND requested_at<=?`, sample.cycle, sample.at).Scan(&oldSQL)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(sample.value-oldSQL) > 1e-9 {
			t.Fatalf("sample %d prefix=%f old SQL=%f", sample.id, sample.value, oldSQL)
		}
	}
}

func TestBackgroundPricingTaskRejectsOverlapAndRollsBackFailure(t *testing.T) {
	s, a := pricingFixture(t)
	defer s.close()
	entered := make(chan struct{})
	release := make(chan struct{})
	a.pricingProgressHook = func(progress pricingRecalcProgress) error {
		if progress.Stage == "samples" && progress.SamplesDone == progress.SamplesTotal {
			close(entered)
			<-release
			return errors.New("injected failure")
		}
		return nil
	}
	body := []byte(`{"pricing_mode":"api"}`)
	first := a.handleManagement(managementRequest{Method: "POST", Path: "/cpa-quota-estimator/pricing-settings", Body: body})
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.StatusCode, first.Body)
	}
	var started pricingRecalcTask
	if err := json.Unmarshal(first.Body, &started); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("task did not enter transaction")
	}
	second := a.handleManagement(managementRequest{Method: "POST", Path: "/cpa-quota-estimator/pricing-settings", Body: body})
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent status=%d", second.StatusCode)
	}
	close(release)
	failed := waitPricingTask(t, a, started.ID)
	if failed.Status != "failed" || failed.Error == "" {
		t.Fatalf("failed task=%#v", failed)
	}
	var eventCost, sampleCost float64
	if err := s.db.QueryRow(`SELECT cost_usd FROM usage_events LIMIT 1`).Scan(&eventCost); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT window_cost_usd FROM quota_samples LIMIT 1`).Scan(&sampleCost); err != nil {
		t.Fatal(err)
	}
	if eventCost != 99 || sampleCost != 99 || a.cfg.PricingMode != pricingModeCredits {
		t.Fatalf("rollback cost=%f sample=%f mode=%s", eventCost, sampleCost, a.cfg.PricingMode)
	}
	var persisted int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM metadata WHERE key=?`, pricingSettingsMetadataKey).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 0 {
		t.Fatalf("failed task persisted settings")
	}
	a.pricingProgressHook = nil
	retry := a.handleManagement(managementRequest{Method: "POST", Path: "/cpa-quota-estimator/pricing-settings", Body: body})
	if retry.StatusCode != http.StatusAccepted {
		t.Fatalf("retry status=%d", retry.StatusCode)
	}
	var next pricingRecalcTask
	if err := json.Unmarshal(retry.Body, &next); err != nil {
		t.Fatal(err)
	}
	done := waitPricingTask(t, a, next.ID)
	if done.Status != "succeeded" || a.cfg.PricingMode != pricingModeAPI {
		t.Fatalf("retry=%#v cfg=%s", done, a.cfg.PricingMode)
	}
}

func TestRestoreOfficialCustomPrices(t *testing.T) {
	s, a := pricingFixture(t)
	defer s.close()
	request := func(body string) {
		response := a.handleManagement(managementRequest{Method: "POST", Path: "/cpa-quota-estimator/pricing-settings", Body: []byte(body)})
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
		}
		var task pricingRecalcTask
		if err := json.Unmarshal(response.Body, &task); err != nil {
			t.Fatal(err)
		}
		if done := waitPricingTask(t, a, task.ID); done.Status != "succeeded" {
			t.Fatalf("task=%#v", done)
		}
	}
	request(`{"pricing_mode":"custom","custom_prices":{"gpt-5.6-sol":{"input":7,"cache_read":0.7,"output":35,"cache_write":8}},"custom_fast_multiplier":3,"custom_long_context":false}`)
	if got := a.cfg.CustomPrices["gpt-5.6-sol"].Input; got != 7 {
		t.Fatalf("custom input=%f", got)
	}
	request(`{"pricing_mode":"custom","restore_official":true}`)
	if len(a.cfg.CustomPrices) != 0 || a.cfg.CustomFastMultiplier != 2 || !a.cfg.CustomLongContext {
		t.Fatalf("restored config=%#v", a.cfg)
	}
	price := a.cfg.PriceCatalog["gpt-5.6-sol"]
	if effective, _ := a.cfg.effectiveModelPrice(price); effective.Input != price.Input {
		t.Fatalf("restored price=%#v official=%#v", effective, price)
	}
}

func TestSavedLearnedModeMigratesAndRepricesOnConfigure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrate.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes) VALUES(1,'a',100,1000,15)`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,account,requested_at,model,input_tokens,total_tokens,cost_usd,quota_scope) VALUES(1,'a',200,'gpt-5.6-sol',1000000,1000000,0.01,'main')`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO quota_samples(cycle_id,account,sampled_at,used_percent,reset_at,window_minutes,window_cost_usd) VALUES(1,'a',200,1,1000,15,0.01)`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO metadata(key,value) VALUES(?,?)`, pricingSettingsMetadataKey, `{"pricing_mode":"learned"}`); err != nil {
		t.Fatal(err)
	}
	s.close()
	a := &app{}
	if err = a.configure([]byte("data_path: " + path + "\nprice_source_url: http://127.0.0.1:1\n")); err != nil {
		t.Fatal(err)
	}
	defer a.shutdown()
	if a.cfg.PricingMode != pricingModeCredits {
		t.Fatalf("migrated mode=%s", a.cfg.PricingMode)
	}
	var eventCost, sampleCost float64
	if err = a.store.db.QueryRow(`SELECT cost_usd FROM usage_events`).Scan(&eventCost); err != nil {
		t.Fatal(err)
	}
	if err = a.store.db.QueryRow(`SELECT window_cost_usd FROM quota_samples`).Scan(&sampleCost); err != nil {
		t.Fatal(err)
	}
	if eventCost != 100 || sampleCost != 100 {
		t.Fatalf("migrated event=%f sample=%f", eventCost, sampleCost)
	}
	saved, err := a.store.loadPricingSettings(context.Background(), defaultConfig().pricingSettings())
	if err != nil || saved.PricingMode != pricingModeCredits || saved.Migrated {
		t.Fatalf("saved=%#v err=%v", saved, err)
	}
}
