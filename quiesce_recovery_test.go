package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func failingQuiesceApp(t *testing.T, persistentFailure bool) (*app, *store, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.DataPath = path
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"providers":{}}`))
	}))
	t.Cleanup(server.Close)
	cfg.PriceSourceURL = server.URL
	a := &app{store: s, cfg: cfg}
	t.Cleanup(a.shutdown)
	query := `PRAGMA query_only=ON`
	if persistentFailure {
		query = `CREATE TRIGGER fail_drop_flush BEFORE INSERT ON metadata
			WHEN NEW.key='dropped_usage_events'
			BEGIN SELECT RAISE(FAIL, 'injected drop flush failure'); END`
	}
	if _, err = s.db.Exec(query); err != nil {
		t.Fatal(err)
	}
	a.droppedUsagePending.Store(3)
	return a, s, []byte("data_path: " + path + "\nprice_source_url: " + server.URL + "\n")
}

func TestQuiesceFailureSurvivesRepeatedStops(t *testing.T) {
	a, s, _ := failingQuiesceApp(t, false)
	first := a.quiesce()
	if first == nil {
		t.Fatal("quiesce succeeded despite failed accounting flush")
	}
	if err := s.db.Ping(); err == nil {
		t.Fatal("failed flush left the old connection open")
	}
	a.shutdown()
	results := make(chan error, 4)
	for i := 0; i < cap(results); i++ {
		go func() { results <- a.quiesce() }()
	}
	for i := 0; i < cap(results); i++ {
		if err := <-results; err == nil || err.Error() != first.Error() {
			t.Fatalf("repeated quiesce error = %v, want %v", err, first)
		}
	}
	if got := a.droppedUsagePending.Load(); got != 3 {
		t.Fatalf("pending count = %d, want 3", got)
	}
}

func TestReconfigureRecoversFailedQuiesceAccounting(t *testing.T) {
	a, old, raw := failingQuiesceApp(t, false)
	if err := a.quiesce(); err == nil {
		t.Fatal("expected the injected flush failure")
	}
	if err := a.configure(raw); err != nil {
		t.Fatalf("recover original store: %v", err)
	}
	a.mu.RLock()
	recovered := a.store
	a.mu.RUnlock()
	if recovered == nil || recovered == old {
		t.Fatal("recovery did not publish a fresh store")
	}
	if got := a.droppedUsagePending.Load(); got != 0 {
		t.Fatalf("reconfigure returned success before flushing pending count: %d", got)
	}
	count, err := recovered.droppedUsageCount(context.Background())
	if err != nil || count != 3 {
		t.Fatalf("persisted count = %d, err = %v; want 3", count, err)
	}
	for i := 0; i < 2; i++ {
		if err = a.quiesce(); err != nil {
			t.Fatalf("stop recovered generation: %v", err)
		}
	}
	if err = a.configure(raw); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	a.mu.RLock()
	recovered = a.store
	a.mu.RUnlock()
	count, err = recovered.droppedUsageCount(context.Background())
	if err != nil || count != 3 {
		t.Fatalf("repeated recovery changed accounting: count=%d err=%v", count, err)
	}
}

func TestReconfigureKeepsAdmissionClosedWhenRecoveryFlushFails(t *testing.T) {
	a, old, raw := failingQuiesceApp(t, true)
	if err := a.quiesce(); err == nil {
		t.Fatal("expected the injected flush failure")
	}
	if err := a.configure(raw); err == nil {
		t.Fatal("reconfigure succeeded while accounting still could not be persisted")
	}
	if !detached(a) {
		t.Fatal("failed recovery admitted store operations")
	}
	if _, err := a.handle("usage.handle", []byte(`{"Model":"gpt","Detail":{"TotalTokens":1}}`)); err == nil {
		t.Fatal("failed recovery accepted usage")
	}
	if err := a.quiesce(); err == nil {
		t.Fatal("failed recovery cleared the quiesce error")
	}
	if got := a.droppedUsagePending.Load(); got != 3 {
		t.Fatalf("failed recovery lost pending count: %d", got)
	}
	// Repair the persistent fault externally, as an operator would, then
	// ensure a new rollback attempt can recover the same database.
	repaired, err := openStore(old.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repaired.db.Exec(`DROP TRIGGER fail_drop_flush`); err != nil {
		repaired.close()
		t.Fatal(err)
	}
	if err = repaired.close(); err != nil {
		t.Fatal(err)
	}
	if err = a.configure(raw); err != nil {
		t.Fatalf("recover after repairing flush failure: %v", err)
	}
	if err = a.quiesce(); err != nil {
		t.Fatalf("quiesce after recovery: %v", err)
	}
}

func TestReconfigureCannotMoveUnflushedAccountingToAnotherDatabase(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		name := "active store"
		if stopped {
			name = "quiesced store"
		}
		t.Run(name, func(t *testing.T) {
			a, old, raw := failingQuiesceApp(t, false)
			if stopped {
				if err := a.quiesce(); err == nil {
					t.Fatal("expected the injected flush failure")
				}
			}
			newPath := filepath.Join(t.TempDir(), "other.sqlite")
			err := a.configure([]byte("data_path: " + newPath + "\nprice_source_url: \"\"\n"))
			if err == nil {
				t.Fatal("reconfigure moved on despite failed accounting in the original store")
			}
			if !strings.Contains(err.Error(), "flush dropped usage count") {
				t.Fatalf("missing flush error: %v", err)
			}
			if !detached(a) || a.droppedUsagePending.Load() != 3 {
				t.Fatal("failed store switch admitted work or lost pending accounting")
			}
			if err = old.db.Ping(); err == nil {
				t.Fatal("original store remained open after failed switch")
			}
			if stopped {
				if _, err = os.Stat(newPath); !os.IsNotExist(err) {
					t.Fatalf("recovery opened a different database: %v", err)
				}
			}
			if err = a.quiesce(); err == nil {
				t.Fatal("stop after failed switch forgot the cleanup error")
			}
			if err = a.configure(raw); err != nil {
				t.Fatalf("recover original database: %v", err)
			}
			a.mu.RLock()
			recovered := a.store
			a.mu.RUnlock()
			count, err := recovered.droppedUsageCount(context.Background())
			if err != nil || count != 3 {
				t.Fatalf("original accounting not restored: count=%d err=%v", count, err)
			}
		})
	}
}
