package main

// 针对本次事故的三组回归测试：
//  1. checkpoint worker 与长时间维护任务解耦（原先同一个 goroutine，拟合期间
//     checkpoint 根本不可能执行）。
//  2. updateWeightAttributions 的 keyset 分页：固定 MAX(id) 上界、取消、basis 切换。
//  3. 长读 + 分页写期间 usage 写入的延迟仍可接受。
//
// 注意：判读 WAL 状态必须看 PRAGMA wal_checkpoint 返回的 busy/log/checkpointed
// 或 -shm 的 mxFrame/nBackfill，-wal 文件大小只是高水位，会原地复用。

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newCheckpointTestStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.close() })
	return s
}

// insertRawUsageEvents writes events without the cycle/segment side effects so a
// test can reach production-like row counts quickly.
func insertRawUsageEvents(t *testing.T, s *store, count int, startAt int64) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_events(requested_at,observed_at,account,provider,model,service_tier,
		input_tokens,output_tokens,total_tokens,cycle_id,reset_at,window_minutes,quota_scope,failed)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,0)`)
	if err != nil {
		tx.Rollback()
		t.Fatalf("prepare: %v", err)
	}
	for i := 0; i < count; i++ {
		at := startAt + int64(i)
		if _, err = stmt.Exec(at, at, "acct-a", "codex", "gpt-6-sol", "default", 1000, 500, 1500, 1, 0, 300, mainQuotaScope); err != nil {
			stmt.Close()
			tx.Rollback()
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err = stmt.Close(); err != nil {
		tx.Rollback()
		t.Fatalf("close stmt: %v", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func walCheckpointDetail(t *testing.T, s *store) (busy, logPages, checkpointed int) {
	t.Helper()
	connection, err := sql.Open("sqlite", s.path)
	if err != nil {
		t.Fatalf("open checkpoint connection: %v", err)
	}
	defer connection.Close()
	connection.SetMaxOpenConns(1)
	if err = connection.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logPages, &checkpointed); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}
	return busy, logPages, checkpointed
}

// The old implementation ran the 30s checkpoint ticker in the same goroutine as
// the weight fit, so a multi-minute fit silenced checkpointing entirely. The
// worker must keep checkpointing while the caller is busy.
func TestCheckpointWorkerRunsDuringLongMaintenance(t *testing.T) {
	s := newCheckpointTestStore(t)
	insertRawUsageEvents(t, s, 3000, time.Now().Unix()-3600)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &app{}
	stopped := make(chan struct{})
	a.workWG.Add(1)
	go func() {
		defer close(stopped)
		a.checkpointWorker(ctx, s, 5*time.Millisecond)
	}()

	// Stand in for a long maintenance pass (fit/rebuild) that owns the caller.
	busyUntil := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(busyUntil) {
		time.Sleep(10 * time.Millisecond)
	}

	runs := s.walCheckpointRuns.Load()
	if runs < 3 {
		t.Fatalf("checkpoint worker ran %d times during maintenance, want >= 3", runs)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("checkpoint worker did not stop on context cancellation")
	}
	a.workWG.Wait()

	busy, logPages, checkpointed := walCheckpointDetail(t, s)
	// Only the PRAGMA counters describe pending work; the -wal file size keeps its
	// high-water mark and is not reported here on purpose.
	t.Logf("checkpoint runs=%d busy=%d log_frames=%d checkpointed=%d", runs, busy, logPages, checkpointed)
}

// A cancellation between read batches must return the rows already written and
// propagate the error instead of silently completing.
func TestUpdateWeightAttributionsStopsOnCancel(t *testing.T) {
	s := newCheckpointTestStore(t)
	insertRawUsageEvents(t, s, 12000, time.Now().Unix()-7200)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	written, err := s.updateWeightAttributions(ctx, &weightFit{}, 272000)
	if err == nil {
		t.Fatalf("expected cancellation error, wrote %d rows", written)
	}
	if written != 0 {
		t.Fatalf("wrote %d rows after cancellation, want 0", written)
	}
}

// sentinelAttribution marks rows the pass has not processed yet. A row that
// still carries the sentinel was provably untouched, unlike a NULL check.
const sentinelAttribution = 42.5

func markAttributionSentinel(t *testing.T, s *store) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE usage_events SET learned_quota_pct=?, learned_secondary_pct=NULL`, sentinelAttribution); err != nil {
		t.Fatalf("mark sentinel: %v", err)
	}
}

// remainingSentinelRows counts rows at or below the pinned bound that the pass
// has not rewritten yet. Rows above the bound are excluded on purpose: they were
// inserted by live traffic after the pin and must stay untouched.
func remainingSentinelRows(t *testing.T, s *store, upperID int64) int64 {
	t.Helper()
	var remaining int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE id<=? AND learned_quota_pct=?`, upperID, sentinelAttribution).Scan(&remaining); err != nil {
		t.Fatalf("count sentinel: %v", err)
	}
	return remaining
}

// The pass must pin MAX(id) when it starts: events inserted by live traffic
// while it runs are attributed by the next fit, not chased by the current one.
// The pin is observed through store.attributionPinned, so the concurrent insert
// happens at a deterministic point instead of racing on a sleep.
func TestUpdateWeightAttributionsPinsUpperBound(t *testing.T) {
	s := newCheckpointTestStore(t)
	insertRawUsageEvents(t, s, 6000, time.Now().Unix()-3600)
	markAttributionSentinel(t, s)

	pinned := make(chan int64, 1)
	inserted := make(chan struct{})
	s.attributionPinned = func(upperID int64) {
		pinned <- upperID
		// Live traffic arrives only after the pass has pinned its upper bound.
		insertRawUsageEvents(t, s, 400, time.Now().Unix())
		markNewRowsSentinel(t, s, upperID)
		close(inserted)
	}
	written, err := s.updateWeightAttributions(context.Background(), &weightFit{}, 272000)
	if err != nil {
		t.Fatalf("attribution pass: %v", err)
	}
	upperID := <-pinned
	<-inserted
	if written != upperID {
		t.Fatalf("attributed %d rows, want the pinned %d", written, upperID)
	}
	var untouchedNew int64
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE id>? AND learned_quota_pct=?`, upperID, sentinelAttribution).Scan(&untouchedNew); err != nil {
		t.Fatal(err)
	}
	if untouchedNew != 400 {
		t.Fatalf("%d of the 400 events past the pinned bound were rewritten, want 0", 400-untouchedNew)
	}
	if remaining := remainingSentinelRows(t, s, upperID); remaining != 0 {
		t.Fatalf("%d rows below the pinned bound were left unattributed", remaining)
	}
}

// markNewRowsSentinel keeps rows inserted by the test distinguishable from rows
// the pass rewrites (their learned_quota_pct defaults to NULL on insert).
func markNewRowsSentinel(t *testing.T, s *store, afterID int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE usage_events SET learned_quota_pct=? WHERE id>?`, sentinelAttribution, afterID); err != nil {
		t.Fatalf("mark concurrent rows: %v", err)
	}
}

// Cancellation must be honoured once a batch has already been committed: the
// call reports exactly the rows that are durable and leaves the rest untouched.
func TestUpdateWeightAttributionsCancelsAfterFirstBatch(t *testing.T) {
	s := newCheckpointTestStore(t)
	insertRawUsageEvents(t, s, 4000, time.Now().Unix()-3600)
	markAttributionSentinel(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var committed int64
	var once sync.Once
	s.attributionCommitted = func(written int64) {
		once.Do(func() {
			committed = written
			cancel()
		})
	}
	written, err := s.updateWeightAttributions(ctx, &weightFit{}, 272000)
	if err == nil {
		t.Fatalf("expected cancellation after the first batch, wrote %d rows", written)
	}
	if committed == 0 {
		t.Fatal("no batch was committed before cancellation")
	}
	if written != committed {
		t.Fatalf("reported %d committed rows, durable batch counted %d", written, committed)
	}
	if remaining := remainingSentinelRows(t, s, 4000); remaining != 4000-committed {
		t.Fatalf("%d rows still carry the sentinel, want %d", remaining, 4000-committed)
	}
}

// A basis switch after the first committed batch must abort the rest of the
// pass instead of writing values computed for the old basis.
func TestUpdateWeightAttributionsSwitchesBasisAfterFirstBatch(t *testing.T) {
	s := newCheckpointTestStore(t)
	insertRawUsageEvents(t, s, 4000, time.Now().Unix()-3600)
	markAttributionSentinel(t, s)
	fit := &weightFit{Available: true, ReferenceModel: "gpt-6-sol", ReferenceInputCredits: 250}
	if _, err := s.db.Exec(`INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		onlineWeightBasisKey, fit.weightBasis()); err != nil {
		t.Fatal(err)
	}
	var committed int64
	var once sync.Once
	s.attributionCommitted = func(written int64) {
		once.Do(func() {
			committed = written
			if _, err := s.db.Exec(`INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
				onlineWeightBasisKey, "switched-basis"); err != nil {
				t.Errorf("switch basis: %v", err)
			}
		})
	}
	written, err := s.updateWeightAttributions(context.Background(), fit, 272000)
	if err == nil {
		t.Fatalf("expected stale-basis abort after the first batch, wrote %d rows", written)
	}
	if committed == 0 {
		t.Fatal("no batch was committed before the basis switch")
	}
	if written != committed {
		t.Fatalf("reported %d committed rows, durable batch counted %d", written, committed)
	}
	if remaining := remainingSentinelRows(t, s, 4000); remaining != 4000-committed {
		t.Fatalf("%d rows still carry the sentinel, want %d", remaining, 4000-committed)
	}
}

// Switching the online weight basis mid-pass must abort instead of writing
// attributions computed for a stale basis, and must report what it committed.
func TestUpdateWeightAttributionsRejectsStaleBasis(t *testing.T) {
	s := newCheckpointTestStore(t)
	insertRawUsageEvents(t, s, 2000, time.Now().Unix()-3600)
	fit := &weightFit{Available: true, ReferenceModel: "gpt-6-sol", ReferenceInputCredits: 250}
	if _, err := s.db.Exec(`INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		onlineWeightBasisKey, "some-other-basis"); err != nil {
		t.Fatal(err)
	}
	written, err := s.updateWeightAttributions(context.Background(), fit, 272000)
	if err == nil {
		t.Fatalf("expected stale-basis rejection, wrote %d rows", written)
	}
	if written != 0 {
		t.Fatalf("wrote %d rows under a stale basis, want 0", written)
	}
}

// Usage writes are the host's hot path: paging the attribution rewrite must not
// stall them while a long read snapshot is open.
func TestUsageWritesStayResponsiveDuringAttribution(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency measurement in -short mode")
	}
	s := newCheckpointTestStore(t)
	insertRawUsageEvents(t, s, 15000, time.Now().Unix()-7200)
	var upperID int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id),0) FROM usage_events`).Scan(&upperID); err != nil {
		t.Fatal(err)
	}
	/// Hold a long read snapshot open, like a running fit does.
	reader, err := s.openReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	readTx, err := reader.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readTx.Rollback()
	var snapshotMax int64
	if err = readTx.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&snapshotMax); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &app{}
	a.workWG.Add(1)
	go a.checkpointWorker(ctx, s, 10*time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := s.updateWeightAttributions(context.Background(), &weightFit{}, 272000)
		done <- err
	}()

	var worst time.Duration
	for i := 0; i < 200; i++ {
		start := time.Now()
		insertRawUsageEvents(t, s, 1, time.Now().Unix()+int64(i))
		if elapsed := time.Since(start); elapsed > worst {
			worst = elapsed
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	a.workWG.Wait()
	if err := <-done; err != nil {
		t.Fatalf("attribution pass: %v", err)
	}
	if worst > 3*time.Second {
		t.Fatalf("worst single usage write took %s, want < 3s", worst)
	}
	busy, logPages, checkpointed := walCheckpointDetail(t, s)
	t.Logf("worst usage write=%s busy=%d log_frames=%d checkpointed=%d",
		worst, busy, logPages, checkpointed)
}
