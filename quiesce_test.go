package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func callLifecycle(t *testing.T, a *app, method string, configYAML []byte) envelope {
	t.Helper()
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: configYAML})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.handle(method, request)
	if err != nil {
		t.Fatalf("%s failed: %v", method, err)
	}
	var env envelope
	if err = json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func waitFor(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func detached(a *app) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.store == nil
}

func TestQuiesceDrainsInFlightWorkAndClosesStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiesce.sqlite")
	raw := []byte("data_path: " + path + "\n")
	a := &app{}
	if env := callLifecycle(t, a, "plugin.register", raw); !env.OK {
		t.Fatalf("register failed: %s", env.Error.Message)
	}
	a.mu.RLock()
	s := a.store
	a.mu.RUnlock()
	if s == nil {
		t.Fatal("store was not configured")
	}

	// Park an in-flight usage call inside the insert writer so quiesce must
	// wait for it to finish before reporting success.
	started := make(chan struct{})
	release := make(chan struct{})
	a.mu.Lock()
	a.insertEventWriter = func(context.Context, event, time.Duration) error {
		close(started)
		<-release
		return nil
	}
	a.cfg.Enabled = true
	a.mu.Unlock()

	usageDone := make(chan error, 1)
	go func() {
		_, err := a.handle("usage.handle", []byte(`{"Model":"gpt","Detail":{"TotalTokens":1}}`))
		usageDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked usage writer did not start")
	}

	quiesceDone := make(chan envelope, 1)
	go func() { quiesceDone <- callLifecycle(t, a, "plugin.quiesce", nil) }()
	select {
	case env := <-quiesceDone:
		t.Fatalf("quiesce returned %v before in-flight usage drained", env.OK)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-usageDone; err != nil {
		t.Fatalf("usage.handle failed: %v", err)
	}
	var env envelope
	select {
	case env = <-quiesceDone:
	case <-time.After(10 * time.Second):
		t.Fatal("quiesce did not return after in-flight work drained")
	}
	if !env.OK {
		t.Fatalf("quiesce failed: %s", env.Error.Message)
	}
	if err := s.db.Ping(); err == nil {
		t.Fatal("old SQLite connection is still open after quiesce")
	}

	// Repeated quiesce is safe.
	if env := callLifecycle(t, a, "plugin.quiesce", nil); !env.OK {
		t.Fatalf("repeated quiesce failed: %v", env.Error)
	}
	// New usage after quiesce must not reach the old store.
	if _, err := a.handle("usage.handle", []byte(`{"Model":"gpt","Detail":{"TotalTokens":1}}`)); err == nil {
		t.Fatal("usage.handle succeeded on quiesced instance")
	}

	// Host rollback: reconfigure restores the old instance.
	if env := callLifecycle(t, a, "plugin.reconfigure", raw); !env.OK {
		t.Fatalf("reconfigure after quiesce failed: %s", env.Error.Message)
	}
	a.mu.Lock()
	restored := a.store
	a.insertEventWriter = nil
	a.mu.Unlock()
	if restored == nil || restored == s {
		t.Fatal("reconfigure did not reopen the store")
	}
	if err := restored.db.Ping(); err != nil {
		t.Fatalf("restored store is not usable: %v", err)
	}
	if _, err := a.handle("usage.handle", []byte(`{"Model":"gpt","Detail":{"TotalTokens":1}}`)); err != nil {
		t.Fatalf("usage.handle after restore failed: %v", err)
	}
	var stored int64
	if err := restored.db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("usage_events rows=%d, want 1", stored)
	}

	// Quiesce again on the restored generation and verify shutdown stays
	// idempotent afterwards.
	if env := callLifecycle(t, a, "plugin.quiesce", nil); !env.OK {
		t.Fatalf("second-generation quiesce failed: %v", env.Error)
	}
	if err := restored.db.Ping(); err == nil {
		t.Fatal("restored SQLite connection is still open after quiesce")
	}
	a.shutdown()
	a.shutdown()
}

func TestQuiesceOnUnconfiguredInstanceIsNoop(t *testing.T) {
	a := &app{}
	env := callLifecycle(t, a, "plugin.quiesce", nil)
	if !env.OK {
		t.Fatalf("quiesce on unconfigured plugin failed: %v", env.Error)
	}
}

// Concurrent drains must all observe the same completed close: the second
// quiesce may not return success while the first still holds the store open.
func TestConcurrentQuiesceAndShutdownWaitForClose(t *testing.T) {
	for _, second := range []string{"quiesce", "shutdown"} {
		t.Run(second, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "concurrent.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			a := &app{store: s, cfg: defaultConfig()}
			started := make(chan struct{})
			release := make(chan struct{})
			s.spawnDetached(func() { close(started); <-release })
			<-started

			first := make(chan error, 1)
			go func() { first <- a.quiesce() }()
			waitFor(t, "first quiesce detached store", func() bool { return detached(a) })

			secondDone := make(chan struct{})
			var secondErr error
			go func() {
				defer close(secondDone)
				if second == "quiesce" {
					secondErr = a.quiesce()
				} else {
					a.shutdown()
				}
			}()
			select {
			case <-secondDone:
				t.Fatalf("concurrent %s finished while first drain still held the store open (ping=%v)", second, s.db.Ping())
			case <-time.After(200 * time.Millisecond):
			}
			close(release)
			if err := <-first; err != nil {
				t.Fatalf("first quiesce failed: %v", err)
			}
			select {
			case <-secondDone:
			case <-time.After(5 * time.Second):
				t.Fatalf("concurrent %s did not finish after drain", second)
			}
			if secondErr != nil {
				t.Fatalf("second quiesce failed: %v", secondErr)
			}
			if err := s.db.Ping(); err == nil {
				t.Fatal("store still open after concurrent drain")
			}
		})
	}
}

// A reconfigure must not reopen the database or admit a new generation while
// a quiesce of the previous generation is still draining.
func TestConfigureWaitsForQuiesceDrain(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "old.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &app{store: s, cfg: defaultConfig(), configured: true, cancel: cancel, workCtx: ctx}

	// Park an old-generation store worker so the drain cannot complete.
	started := make(chan struct{})
	release := make(chan struct{})
	s.spawnDetached(func() { close(started); <-release })
	<-started

	quiesced := make(chan error, 1)
	go func() { quiesced <- a.quiesce() }()
	waitFor(t, "quiesce detached store", func() bool { return detached(a) })

	newPath := filepath.Join(t.TempDir(), "new.sqlite")
	configured := make(chan error, 1)
	go func() { configured <- a.configure([]byte("data_path: " + newPath + "\n")) }()
	select {
	case err := <-configured:
		t.Fatalf("configure returned %v while quiesce was still draining", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-quiesced; err != nil {
		t.Fatalf("quiesce failed: %v", err)
	}
	select {
	case err := <-configured:
		if err != nil {
			t.Fatalf("configure after quiesce failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("configure did not complete after quiesce drained")
	}
	a.mu.RLock()
	restored := a.store
	a.mu.RUnlock()
	if restored == nil || restored == s {
		t.Fatal("configure did not open a new store generation")
	}
	if err := restored.db.Ping(); err != nil {
		t.Fatalf("new store unusable: %v", err)
	}
	if err := s.db.Ping(); err == nil {
		t.Fatal("old store still open after drain")
	}
	if env := callLifecycle(t, a, "plugin.quiesce", nil); !env.OK {
		t.Fatalf("quiesce on restored generation failed: %v", env.Error)
	}
}

// Management endpoints that drop a.mu before finishing DB work must still be
// drained by quiesce.
func TestQuiesceDrainsManagementExceptions(t *testing.T) {
	t.Run("prices sync", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
			fmt.Fprint(w, `{"providers":{"openai":{"models":{"gpt-review":{"cost":{"input":1,"output":2}}}}}}`)
		}))
		defer server.Close()
		s, err := openStore(filepath.Join(t.TempDir(), "sync.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := defaultConfig()
		cfg.PriceSourceURL = server.URL
		a := &app{store: s, cfg: cfg}

		request := make(chan managementResponse, 1)
		go func() { request <- a.handleManagement(managementRequest{Method: "POST", Path: "/prices/sync"}) }()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("price sync request did not reach test server")
		}
		stopped := make(chan error, 1)
		go func() { stopped <- a.quiesce() }()
		select {
		case err := <-stopped:
			t.Fatalf("quiesce returned %v while management price sync was blocked", err)
		case <-time.After(200 * time.Millisecond):
		}
		close(release)
		result := <-request
		if result.StatusCode != http.StatusOK {
			t.Fatalf("price sync status=%d body=%s", result.StatusCode, result.Body)
		}
		if err := <-stopped; err != nil {
			t.Fatalf("quiesce failed: %v", err)
		}
		if err := s.db.Ping(); err == nil {
			t.Fatal("store closed before price sync completed")
		}
	})

	t.Run("early reset repair", func(t *testing.T) {
		s, err := openStore(filepath.Join(t.TempDir(), "repair.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		a := &app{store: s, cfg: defaultConfig()}
		// Hold the single SQLite connection so the repair op is provably
		// admitted and blocked on real DB work (WaitCount) before quiesce.
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan managementResponse, 1)
		go func() {
			done <- a.handleManagement(managementRequest{
				Method: "POST", Path: "/cpa-quota-estimator/repair/early-resets",
				Query: url.Values{"account": {"acc"}},
			})
		}()
		waitFor(t, "repair op blocked on database connection", func() bool {
			return s.db.Stats().WaitCount > 0
		})
		stopped := make(chan error, 1)
		go func() { stopped <- a.quiesce() }()
		select {
		case err := <-stopped:
			t.Fatalf("quiesce returned %v while management repair was blocked", err)
		case <-time.After(200 * time.Millisecond):
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("repair response missing")
		}
		if err := <-stopped; err != nil {
			t.Fatalf("quiesce after repair failed: %v", err)
		}
		if err := s.db.Ping(); err == nil {
			t.Fatal("store still open after quiesce")
		}
	})
}

// Reconfigure must seal store admission while the previous generation drains.
func TestConfigureRejectsStoreWorkWhileOldGenerationDrains(t *testing.T) {
	old, err := openStore(filepath.Join(t.TempDir(), "old-admission.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"providers":{"openai":{"models":{"gpt-review":{"cost":{"input":1,"output":2}}}}}}`))
	}))
	defer server.Close()
	cfg := defaultConfig()
	cfg.DataPath = old.path
	canceled := make(chan struct{})
	a := &app{store: old, cfg: cfg, cancel: func() { close(canceled) }, workCtx: context.Background()}
	a.mu.RLock()
	finishOldOperation := a.beginStoreOp()
	a.mu.RUnlock()
	configured := make(chan error, 1)
	go func() {
		configured <- a.configure([]byte("data_path: " + filepath.Join(t.TempDir(), "new-admission.sqlite") + "\nprice_source_url: " + server.URL + "\n"))
	}()
	defer func() {
		finishOldOperation()
		select {
		case err := <-configured:
			if err != nil {
				t.Errorf("configure after drain: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("configure did not finish after old operation drained")
		}
		a.shutdown()
	}()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("configure did not start draining the old generation")
	}
	response := a.handleManagement(managementRequest{Method: "POST", Path: "/prices/sync"})
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("store work admitted during old-generation drain: status %d, want 503", response.StatusCode)
	}
}

// A pricing-migration scheduling failure must not leak the reserved
// background-worker count and wedge quiesce.
func TestConfigureMigrationFailureLeavesDrainable(t *testing.T) {
	old, err := openStore(filepath.Join(t.TempDir(), "old.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &app{store: old, cfg: defaultConfig(), configured: true, cancel: cancel, workCtx: ctx}

	// Park old-store close so admission stays sealed while we inject an
	// active pricing task that rejects the later migration launch.
	release := make(chan struct{})
	closed := make(chan struct{})
	old.spawnDetached(func() { <-release; close(closed) })

	newPath := filepath.Join(t.TempDir(), "migration.sqlite")
	configured := make(chan error, 1)
	go func() {
		configured <- a.configure([]byte("data_path: " + newPath + "\npricing_mode: legacy_api\n"))
	}()
	waitFor(t, "old store detached", func() bool {
		a.mu.RLock()
		defer a.mu.RUnlock()
		return a.store == nil
	})
	a.pricingTaskMu.Lock()
	a.pricingTask = &pricingRecalcTask{ID: "injected", Status: "running"}
	a.pricingTaskMu.Unlock()
	close(release)
	<-closed

	if err := <-configured; err == nil {
		t.Fatal("migration launch unexpectedly succeeded")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- a.quiesce() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("quiesce after failed configure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("quiesce hung: leaked worker reservation")
	}
}

// A failed pending-drop flush must still close the store and report the
// error instead of returning silent success with memory-only data.
func TestQuiesceReportsPendingDropFlushFailure(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "flush.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	a := &app{store: s, cfg: defaultConfig()}
	if _, err := s.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	a.droppedUsagePending.Store(3)
	err = a.quiesce()
	if err == nil {
		t.Fatal("quiesce reported success with dropped-usage count retained only in memory")
	}
	if a.droppedUsagePending.Load() != 3 {
		t.Fatalf("pending count = %d, want restored 3", a.droppedUsagePending.Load())
	}
	if ping := s.db.Ping(); ping == nil {
		t.Fatal("store left open after failed flush")
	}
}
