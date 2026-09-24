package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageRetryIsIdempotentAfterAmbiguousCommit(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "retry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{store: s, cfg: defaultConfig(), usageRetryBudget: time.Second}
	calls := 0
	a.insertEventWriter = func(ctx context.Context, e event, interval time.Duration) error {
		calls++
		if calls == 1 {
			if err := s.insertEvent(ctx, e, interval); err != nil {
				return err
			}
			return context.DeadlineExceeded
		}
		return s.insertEvent(ctx, e, interval)
	}
	e := event{IngestID: newUsageIngestID(), Account: "a", Model: "gpt-5.6-sol", RequestedAt: 100, ObservedAt: 100, TotalTokens: 10}
	if err = a.insertUsageWithRetry(s, e, time.Minute); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("retry calls=%d", calls)
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("ambiguous commit duplicated event: %d", count)
	}
	if a.droppedUsagePending.Load() != 0 {
		t.Fatal("successful retry counted as dropped")
	}
}

func TestUnrecoverableUsageFailurePersistsDropCount(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "drop.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{store: s, cfg: defaultConfig(), usageRetryBudget: 80 * time.Millisecond}
	a.insertEventWriter = func(context.Context, event, time.Duration) error { return errors.New("SQLITE_BUSY") }
	e := event{IngestID: newUsageIngestID(), Account: "a", RequestedAt: 100, ObservedAt: 100}
	if err = a.insertUsageWithRetry(s, e, time.Minute); err == nil {
		t.Fatal("busy writer unexpectedly succeeded")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stored, err := s.droppedUsageCount(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if stored == 1 && a.droppedUsagePending.Load() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("drop count not persisted: pending=%d", a.droppedUsagePending.Load())
}
