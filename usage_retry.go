package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

const droppedUsageMetadataKey = "dropped_usage_events"

var fallbackIngestCounter atomic.Uint64

func newUsageIngestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return hex.EncodeToString(bytes[:])
	}
	return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), fallbackIngestCounter.Add(1))
}

func retryableUsageWrite(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{"busy", "locked", "deadline exceeded", "context canceled", "usage_events.ingest_id"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func (a *app) insertUsageWithRetry(s *store, e event, interval time.Duration) error {
	budget := a.usageRetryBudget
	if budget <= 0 {
		budget = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	writer := a.insertEventWriter
	if writer == nil {
		writer = s.insertEvent
	}
	var lastErr error
	backoff := 50 * time.Millisecond
	for {
		deadline, _ := ctx.Deadline()
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		attemptLimit := min(8*time.Second, remaining)
		attemptCtx, attemptCancel := context.WithTimeout(ctx, attemptLimit)
		lastErr = writer(attemptCtx, e, interval)
		attemptCancel()
		if lastErr == nil {
			a.startDropFlush(s)
			return nil
		}
		if !retryableUsageWrite(lastErr) {
			break
		}
		if time.Until(deadline) <= 0 {
			break
		}
		pause := min(backoff, remaining)
		select {
		case <-ctx.Done():
			break
		case <-time.After(pause):
		}
		backoff = min(backoff*2, 500*time.Millisecond)
		if ctx.Err() != nil {
			break
		}
	}
	a.droppedUsagePending.Add(1)
	a.startDropFlush(s)
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return fmt.Errorf("usage event could not be stored after retries: %w", lastErr)
}

func (s *store) addDroppedUsageCount(ctx context.Context, count int64) error {
	if count <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(metadata.value AS INTEGER)+? AS TEXT)`, droppedUsageMetadataKey, fmt.Sprint(count), count)
	return err
}

func (s *store) droppedUsageCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM metadata WHERE key=?`, droppedUsageMetadataKey).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return count, err
}

func (a *app) totalDroppedUsage(ctx context.Context, s *store) int64 {
	stored, _ := s.droppedUsageCount(ctx)
	return stored + a.droppedUsagePending.Load()
}

func (a *app) startDropFlush(s *store) {
	if a.droppedUsagePending.Load() <= 0 || !a.dropFlushRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.dropFlushRunning.Store(false)
		for attempt := 0; attempt < 5; attempt++ {
			count := a.droppedUsagePending.Swap(0)
			if count <= 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.addDroppedUsageCount(ctx, count)
			cancel()
			if err == nil {
				continue
			}
			a.droppedUsagePending.Add(count)
			time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
		}
	}()
}
