package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func rawUsageFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM usage_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for i := range values {
		targets[i] = &values[i]
	}
	hash := sha256.New()
	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		for i, name := range columns {
			if name != "cycle_id" {
				fmt.Fprintf(hash, "%s:%T:%d:%v\n", name, values[i], len(fmt.Sprint(values[i])), values[i])
			}
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

// Opt-in validation against the unmodified online backup. Never open the
// supplied file through openStore: only its temporary copy may be migrated.
func TestOverlappingResetProductionSnapshot(t *testing.T) {
	snapshot := os.Getenv("CPA_RESET_SNAPSHOT")
	if snapshot == "" {
		t.Skip("set CPA_RESET_SNAPSHOT to the 2026-10-24 online backup")
	}
	source, err := os.Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
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
	before, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	beforeHash := rawUsageFingerprint(t, before)
	var cycleCount, eventCount int64
	var account string
	if err = before.QueryRow(`SELECT COUNT(*) FROM quota_cycles`).Scan(&cycleCount); err != nil {
		t.Fatal(err)
	}
	if err = before.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err = before.QueryRow(`SELECT account FROM quota_cycles WHERE id=23 AND ended_at=0 AND reset_at=?`, overlapNewReset).Scan(&account); err != nil {
		t.Fatal("snapshot does not contain the diagnosed merged cycle:", err)
	}
	var newTokens, newRequests, oldTokens, oldRequests int64
	if err = before.QueryRow(`SELECT COALESCE(SUM(total_tokens),0),COUNT(*) FROM usage_events
WHERE cycle_id=23 AND quota_scope='main' AND requested_at>=?`, overlapFirst).Scan(&newTokens, &newRequests); err != nil {
		t.Fatal(err)
	}
	if err = before.QueryRow(`SELECT COALESCE(SUM(total_tokens),0),COUNT(*) FROM usage_events
WHERE cycle_id=23 AND quota_scope='main' AND requested_at<?`, overlapFirst).Scan(&oldTokens, &oldRequests); err != nil {
		t.Fatal(err)
	}
	if err = before.Close(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	t.Logf("snapshot migration and segment rebuild: %s", time.Since(started))
	var afterCycles, afterEvents int64
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_cycles`).Scan(&afterCycles); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&afterEvents); err != nil {
		t.Fatal(err)
	}
	if afterCycles != cycleCount+1 || afterEvents != eventCount {
		t.Fatalf("unexpected ledger changes: cycles=%d->%d events=%d->%d", cycleCount, afterCycles, eventCount, afterEvents)
	}
	cycles, err := s.cycles(context.Background(), account, 2)
	if err != nil || len(cycles) != 2 {
		t.Fatalf("could not read repaired cycles: %v", err)
	}
	current, previous := cycles[0], cycles[1]
	if previous.ID != 23 || previous.EndedAt != overlapFirst || previous.ResetAt != overlapOldReset ||
		previous.PeakPercent != 65 || previous.ActualTokens != oldTokens || previous.Requests != oldRequests ||
		current.StartedAt != overlapFirst || current.ResetAt != overlapNewReset ||
		current.ActualTokens != newTokens || current.Requests != newRequests {
		t.Fatalf("snapshot split does not conserve old/new ledgers: old=%#v new=%#v", previous, current)
	}
	if afterHash := rawUsageFingerprint(t, s.db); afterHash != beforeHash {
		t.Fatalf("raw usage fields changed: before=%s after=%s", beforeHash, afterHash)
	}
	var staleSegments, badSamples int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_segments q
JOIN usage_events u ON u.id=q.end_event_id
WHERE q.window='main' AND q.cycle_id<>u.cycle_id`).Scan(&staleSegments); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples q WHERE q.cycle_id IN (23,?) AND (
q.window_tokens<>(SELECT COALESCE(SUM(total_tokens),0) FROM usage_events u WHERE u.cycle_id=q.cycle_id AND u.quota_scope='main' AND u.requested_at<=q.sampled_at)
OR q.requests<>(SELECT COUNT(*) FROM usage_events u WHERE u.cycle_id=q.cycle_id AND u.quota_scope='main' AND u.requested_at<=q.sampled_at))`, current.ID).Scan(&badSamples); err != nil {
		t.Fatal(err)
	}
	if staleSegments != 0 || badSamples != 0 {
		t.Fatalf("stale derived data: segments=%d samples=%d", staleSegments, badSamples)
	}
	var integrity string
	if err = s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check=%q err=%v", integrity, err)
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_cycles`).Scan(&afterCycles); err != nil || afterCycles != cycleCount+1 {
		t.Fatalf("reopening repeated the repair: cycles=%d err=%v", afterCycles, err)
	}
	t.Logf("raw SHA-256=%s; events=%d; old tokens=%d requests=%d; new tokens=%d requests=%d",
		beforeHash, eventCount, oldTokens, oldRequests, newTokens, newRequests)
}
