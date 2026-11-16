package main

// 回归测试：quiesce/close 之后不得残留数据库文件句柄。
// 症状来源：Windows CI 的 t.TempDir RemoveAll “file in use”，以及 t1y 在 Linux
// 上 97/100 次复现的 4 个残留 fd。根因是 database/sql 对已取消事务的异步回滚
// 可以越过 Close 返回。测试只轮询真实状态，不靠固定 sleep。

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fdHandlesUnder(dir string) []string {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	var hits []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(target, dir) {
			hits = append(hits, e.Name()+" -> "+target)
		}
	}
	return hits
}

// assertDatabaseRemovable verifies the Windows-visible symptom: every database
// file must be deletable immediately after close.
func assertDatabaseRemovable(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("cannot remove %s%s right after close: %v", path, suffix, err)
		}
	}
}

func TestStoreCloseDrainsCanceledTransactions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		t.Fatalf("begin tx: %v", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('drain-probe','1')`); err != nil {
		cancel()
		t.Fatalf("insert: %v", err)
	}
	cancel()
	runtime.Gosched()
	_ = tx.Rollback() // returns ErrTxDone once the cancellation already rolled back

	if err = s.close(); err != nil {
		t.Fatalf("close returned with pending connections: %v", err)
	}
	if stats := s.db.Stats(); stats.OpenConnections != 0 || stats.InUse != 0 {
		t.Fatalf("after close open=%d inUse=%d", stats.OpenConnections, stats.InUse)
	}

	if runtime.GOOS == "linux" {
		if hits := fdHandlesUnder(dir); len(hits) > 0 {
			t.Fatalf("database handles still open after close: %v", hits)
		}
	}
	assertDatabaseRemovable(t, path)
}

func TestReaderCloseDrainsCanceledTransactions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	reader, err := s.openReadOnly()
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		t.Fatalf("begin reader tx: %v", err)
	}
	if _, err = tx.QueryContext(ctx, `SELECT COUNT(*) FROM usage_events`); err != nil {
		cancel()
		t.Fatalf("query: %v", err)
	}
	cancel()
	runtime.Gosched()
	_ = tx.Rollback()
	if err = closeDBDrained(reader); err != nil {
		t.Fatalf("reader close left connections open: %v", err)
	}
	if stats := reader.Stats(); stats.OpenConnections != 0 || stats.InUse != 0 {
		t.Fatalf("reader after close open=%d inUse=%d", stats.OpenConnections, stats.InUse)
	}

	// The store itself still owns handles; close it before inspecting the files.
	if err = s.close(); err != nil {
		t.Fatalf("store close: %v", err)
	}
	if runtime.GOOS == "linux" {
		if hits := fdHandlesUnder(dir); len(hits) > 0 {
			t.Fatalf("reader database handles still open after close: %v", hits)
		}
	}
	assertDatabaseRemovable(t, path)
}

// A temporary handle that cannot be drained must make the store close fail, so
// quiesce can never report a clean shutdown while a driver may still own a file.
func TestStoreCloseFailsWhenTemporaryHandleLeaks(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(filepath.Join(dir, "usage.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	reader, err := s.openReadOnly()
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	// Keep a connection in use so the drained close cannot reach zero.
	ctx := context.Background()
	tx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	// Release the connection even when an assertion below fails.
	defer func() { _ = tx.Rollback() }()
	var one int
	if err = tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	previous := dbDrainTimeout
	dbDrainTimeout = 50 * time.Millisecond
	defer func() { dbDrainTimeout = previous }()
	s.closeTemporary(reader, "reader")
	if failures, _ := s.drainDiagnostics(); failures == 0 {
		t.Fatal("leaked temporary handle was not recorded")
	}
	if err = s.close(); err == nil {
		t.Fatal("store close reported a clean shutdown while a temporary handle leaked")
	}
}
