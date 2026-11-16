package main

// 内存剖析探针 v2（修正 v1 的方法论缺陷）：
//   - 快照副本由**被测进程之外**准备（shell 里的 cp，流式），进程内不再 ReadFile 大文件；
//   - 每个阶段跑在**独立子进程**里（`go test -c` 出的二进制 + MEMPROBE_PHASE），
//     避免前一阶段的高水位污染后一阶段；
//   - 每阶段使用同一份干净快照副本；
//   - 峰值同时记录 runtime.MemStats 和 /proc/self/statm 的 RSS。
//
// 用法：
//   go test -c -o work/memprobe.test .
//   for ph in open segments backtest pricing attribution rebuild dashboard insert cleanup; do
//     cp work/snapshot.sqlite work/probe.sqlite
//     MEMPROBE_PHASE=$ph MEMPROBE_DB=work/probe.sqlite work/memprobe.test -test.run TestMemProbe -test.v
//   done
//
// 注意：WAL 判读不要看文件大小（高水位会原地复用），要看
// PRAGMA wal_checkpoint 的 busy/log/checkpointed 或 -shm 的 mxFrame/nBackfill。

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type probePeak struct {
	peakHeap uint64
	peakSys  uint64
	peakRSS  uint64
}

func probeRSS() uint64 {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0
	}
	var pages uint64
	fmt.Sscan(fields[1], &pages)
	return pages * uint64(os.Getpagesize())
}

// startProbeSampler records the peak heap/RSS of this process only. It never
// reads the database snapshot itself, so the numbers belong to the code under
// test rather than to the measurement harness.
func startProbeSampler() (stop func() probePeak) {
	var mu sync.Mutex
	peak := probePeak{}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var ms runtime.MemStats
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&ms)
				rss := probeRSS()
				mu.Lock()
				if ms.HeapAlloc > peak.peakHeap {
					peak.peakHeap = ms.HeapAlloc
				}
				if ms.Sys > peak.peakSys {
					peak.peakSys = ms.Sys
				}
				if rss > peak.peakRSS {
					peak.peakRSS = rss
				}
				mu.Unlock()
			}
		}
	}()
	return func() probePeak {
		close(done)
		<-finished
		mu.Lock()
		defer mu.Unlock()
		return peak
	}
}

func TestMemProbe(t *testing.T) {
	phase := os.Getenv("MEMPROBE_PHASE")
	dbPath := os.Getenv("MEMPROBE_DB")
	if phase == "" || dbPath == "" {
		t.Skip("set MEMPROBE_PHASE and MEMPROBE_DB (see file header)")
	}
	ctx := context.Background()
	cfg := defaultConfig()

	stop := startProbeSampler()
	started := time.Now()
	s, err := openStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.close()
	accounts, err := s.accounts(ctx)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("accounts: %v (%d)", err, len(accounts))
	}
	base := &app{store: s, cfg: cfg}
	if fit, ok, err := s.latestWeightFit(ctx); err == nil && ok {
		base.cfg.LearnedFit = &fit.FittedWeights
	}

	switch phase {
	case "open":
		// nothing: measures open + accounts + latest fit only
	case "segments":
		for lag := 0; lag <= 2; lag++ {
			segs, err := s.quotaSegments(ctx, lag)
			if err != nil {
				t.Fatalf("quotaSegments lag=%d: %v", lag, err)
			}
			t.Logf("lag=%d segments=%d", lag, len(segs))
		}
	case "backtest":
		byLag := make(map[int][]quotaSegment, 3)
		for lag := 0; lag <= 2; lag++ {
			segs, err := s.quotaSegments(ctx, lag)
			if err != nil {
				t.Fatalf("quotaSegments: %v", err)
			}
			byLag[lag] = segs
		}
		prices, err := s.listPrices(ctx)
		if err != nil {
			t.Fatalf("listPrices: %v", err)
		}
		byModel := make(map[string]price, len(prices))
		for _, p := range prices {
			byModel[normalizeModel(p.Model)] = p
		}
		opts := cfg.weightLearnerOptions()
		opts.ctx = ctx
		if _, err = rollingBacktestProgress(byLag, byModel, time.Now().Unix(), opts, nil); err != nil {
			t.Fatalf("rollingBacktestProgress: %v", err)
		}
	case "pricing":
		snap, err := s.preparePricingSnapshot(ctx, cfg)
		if err != nil {
			t.Fatalf("preparePricingSnapshot: %v", err)
		}
		t.Logf("events=%d oldSamples=%d cycleIDs=%d", len(snap.Events), len(snap.OldSamples), len(snap.CycleIDs))
	case "attribution":
		written, err := s.updateWeightAttributions(ctx, &weightFit{}, 272000)
		if err != nil {
			t.Fatalf("updateWeightAttributions: %v", err)
		}
		t.Logf("attributed=%d", written)
	case "rebuild":
		if err = s.rebuildAllQuotaSegments(ctx); err != nil {
			t.Fatalf("rebuildAllQuotaSegments: %v", err)
		}
	case "dashboard":
		for _, endpoint := range []string{"overview", "summary", "series", "monthly", "usage", "weights", "weights/backtest", "prices"} {
			resp := base.handleManagement(managementRequest{Method: "GET", Path: "/" + endpoint, Query: url.Values{"account": {accounts[0]}, "limit": {"5000"}}})
			if resp.StatusCode != 200 {
				t.Fatalf("%s -> %d", endpoint, resp.StatusCode)
			}
			t.Logf("%s %d KiB", endpoint, len(resp.Body)/1024)
		}
	case "insert":
		now := time.Now().Unix()
		for i := 0; i < 300; i++ {
			used := float64(i%100) + 0.5
			e := event{
				IngestID:      fmt.Sprintf("memprobe-%d", i),
				RequestedAt:   now + int64(i),
				ObservedAt:    now + int64(i),
				Account:       accounts[0],
				Provider:      "codex",
				Model:         "gpt-6-sol",
				ServiceTier:   "default",
				InputTokens:   1200,
				OutputTokens:  800,
				TotalTokens:   2000,
				CostUSD:       0.01,
				UsedPercent:   &used,
				ResetAt:       now + 3600,
				WindowMinutes: 300,
				QuotaScope:    mainQuotaScope,
			}
			if err = s.insertEvent(ctx, e, 30*time.Second); err != nil {
				t.Fatalf("insertEvent %d: %v", i, err)
			}
		}
	case "cleanup":
		if err = s.cleanup(ctx, 30); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if err = s.checkpointWAL(ctx); err != nil {
			t.Fatalf("checkpointWAL: %v", err)
		}
	default:
		t.Fatalf("unknown phase %q", phase)
	}

	peak := stop()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	busy, logPages, checkpointed := 0, 0, 0
	conn, err := sql.Open("sqlite", dbPath)
	if err == nil {
		conn.SetMaxOpenConns(1)
		_ = conn.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logPages, &checkpointed)
		conn.Close()
	}
	t.Logf("PHASE %-12s dur=%6.1fs peakHeap=%6.1fMB peakSys=%6.1fMB peakRSS=%6.1fMB | wal busy=%d log=%d checkpointed=%d",
		phase, time.Since(started).Seconds(),
		float64(peak.peakHeap)/1048576, float64(peak.peakSys)/1048576, float64(peak.peakRSS)/1048576,
		busy, logPages, checkpointed)
}
