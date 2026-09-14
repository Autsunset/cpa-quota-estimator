package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

const (
	advancedResetAt     = int64(1789200720)
	advancedOldReset    = advancedResetAt + 3*24*60*60
	advancedNewReset    = advancedResetAt + 7*24*60*60
	advancedTestAccount = "advanced-reset-synthetic"
)

func insertAdvancedResetEvent(t *testing.T, s *store, offset int64, used float64, reset int64, failed bool) {
	t.Helper()
	if err := s.insertEvent(context.Background(), event{
		Account: advancedTestAccount, RequestedAt: advancedResetAt + offset, ObservedAt: advancedResetAt + offset,
		UsedPercent: &used, ResetAt: reset, WindowMinutes: 10080, PlanType: "pro",
		TotalTokens: 100, CostUSD: 1, Failed: failed,
	}, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestAdvancedEarlyResetWithSparseObservations(t *testing.T) {
	for _, tc := range []struct {
		name string
		used [3]float64
	}{
		{"issue_2_9_10", [3]float64{2, 9, 10}},
		{"first_observation_above_five", [3]float64{9, 10, 11}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sparse.sqlite")
			s, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.close() }()
			ctx := context.Background()
			insertAdvancedResetEvent(t, s, -3600, 80, advancedOldReset, false)
			for i, offset := range []int64{120, 7200, 7260} {
				insertAdvancedResetEvent(t, s, offset, tc.used[i], advancedNewReset, false)
				if i < 2 {
					cycles, err := s.cycles(ctx, advancedTestAccount, 10)
					if err != nil {
						t.Fatal(err)
					}
					if len(cycles) != 1 || cycles[0].ResetAt != advancedOldReset || cycles[0].PeakPercent != 80 {
						t.Fatalf("pending evidence changed the old baseline: %#v", cycles)
					}
					if err = s.close(); err != nil {
						t.Fatal(err)
					}
					s, err = openStore(path)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			cycles, err := s.cycles(ctx, advancedTestAccount, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(cycles) != 2 {
				t.Fatalf("confirmed new window was not split: %#v", cycles)
			}
			current, previous := cycles[0], cycles[1]
			if current.StartedAt != advancedResetAt+120 || current.FirstSampleAt != current.StartedAt ||
				current.StartPercent != tc.used[0] || current.ResetAt != advancedNewReset ||
				current.ActualTokens != 300 || current.ActualCostUSD != 3 || current.Requests != 3 {
				t.Fatalf("new cycle must start at its first real observation: %#v", current)
			}
			if previous.ResetAt != advancedOldReset || previous.EndedAt != current.StartedAt ||
				previous.CloseReason != "early_reset" || previous.PeakPercent != 80 || previous.ActualTokens != 100 {
				t.Fatalf("previous cycle was not preserved: %#v", previous)
			}
			confirmed, _, err := s.historicalEarlyResetConfirmed(ctx, previous, current)
			if err != nil || !confirmed {
				t.Fatalf("historical evidence disagrees with live confirmation: confirmed=%v err=%v", confirmed, err)
			}
			insertAdvancedResetEvent(t, s, 7320, tc.used[2]+1, advancedNewReset, false)
			points, _, err := s.pointsForCycle(ctx, advancedTestAccount, current.ID, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(points) != 3 || points[0].UsedPercent != tc.used[0] || points[0].WindowTokens != 100 ||
				points[2].UsedPercent != tc.used[2]+1 || points[2].WindowTokens != 400 {
				t.Fatalf("new cycle samples = %#v", points)
			}
			month, err := s.monthly(ctx, advancedTestAccount, "2026-09")
			if err != nil {
				t.Fatal(err)
			}
			if month.EarlyResetCount != 1 || month.ActualTokens != 500 || month.Requests != 5 {
				t.Fatalf("monthly totals = %#v", month)
			}
		})
	}
}

func TestAdvancedEarlyResetLaterRevertsToOriginalPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.close() }()
	ctx := context.Background()
	insertAdvancedResetEvent(t, s, -3600, 79, advancedOldReset, false)
	insertAdvancedResetEvent(t, s, -1800, 80, advancedOldReset, false)
	for i, offset := range []int64{120, 7200, 7260} {
		insertAdvancedResetEvent(t, s, offset, []float64{2, 9, 10}[i], advancedNewReset, false)
	}
	cycles, err := s.cycles(ctx, advancedTestAccount, 10)
	if err != nil || len(cycles) != 2 {
		t.Fatalf("expected an inferred early reset before recovery: cycles=%#v err=%v", cycles, err)
	}
	originalID := cycles[1].ID
	insertAdvancedResetEvent(t, s, 7320, 81, advancedOldReset, false)
	cycles, err = s.cycles(ctx, advancedTestAccount, 10)
	if err != nil || len(cycles) != 2 || cycles[0].ResetAt != advancedNewReset {
		t.Fatalf("one possibly stale response must not undo a reset: cycles=%#v err=%v", cycles, err)
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	insertAdvancedResetEvent(t, s, 7380, 82, advancedOldReset, false)
	cycles, err = s.cycles(ctx, advancedTestAccount, 10)
	if err != nil || len(cycles) != 1 {
		t.Fatalf("confirmed rollback must restore a single cycle: cycles=%#v err=%v", cycles, err)
	}
	cycle := cycles[0]
	if cycle.ID != originalID || cycle.EndedAt != 0 || cycle.CloseReason != "" || cycle.ResetAt != advancedOldReset ||
		cycle.StartPercent != 79 || cycle.EndPercent != 82 || cycle.PeakPercent != 82 || cycle.ActualTokens != 700 || cycle.Requests != 7 {
		t.Fatalf("restored original cycle = %#v", cycle)
	}
	anomalies, err := s.quotaRegimeAnomalies(ctx, cycle.ID)
	if err != nil || len(anomalies) != 1 {
		t.Fatalf("rollback evidence must survive the inferred boundary: anomalies=%#v err=%v", anomalies, err)
	}
	if anomalies[0].Kind != quotaRegimeReverted || anomalies[0].BeforeUsedPercent != 80 ||
		anomalies[0].AnomalousUsedPercent != 2 || anomalies[0].PeakUsedPercent != 10 || anomalies[0].RestoredUsedPercent != 81 {
		t.Fatalf("rollback anomaly = %#v", anomalies[0])
	}
	points, _, err := s.pointsForCycle(ctx, advancedTestAccount, cycle.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var recovery *quotaPoint
	for i := range points {
		if points[i].Time == advancedResetAt+7320 {
			recovery = &points[i]
		}
	}
	if recovery == nil || !recovery.BreakBefore || recovery.Anomalous || recovery.WindowTokens != 600 {
		t.Fatalf("recovery boundary = %#v", recovery)
	}
	estimate := estimateCapacity(points)
	if !estimate.Available || estimate.FullWindowTokens != 10000 || estimate.FullWindowCostUSD != 100 {
		t.Fatalf("anomalous low readings contaminated estimation: %#v", estimate)
	}
	month, err := s.monthly(ctx, advancedTestAccount, "2026-09")
	if err != nil || month.EarlyResetCount != 0 || month.ResetCount != 0 || month.ActualTokens != 700 || month.Requests != 7 {
		t.Fatalf("rollback monthly totals = %#v err=%v", month, err)
	}
}

func TestAdvancedEarlyResetPreservesKnownLaterPlanRecovery(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "known-plan.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	// Recovery can move reset_at forward too. A previously observed plan must
	// not be mistaken for a freshly allocated window solely by its direction.
	insertAdvancedResetEvent(t, s, -240, 9, advancedNewReset, false)
	insertAdvancedResetEvent(t, s, -180, 10, advancedNewReset, false)
	insertAdvancedResetEvent(t, s, -120, 80, advancedOldReset, false)
	insertAdvancedResetEvent(t, s, -60, 81, advancedOldReset, false)
	insertAdvancedResetEvent(t, s, 120, 11, advancedNewReset, false)
	insertAdvancedResetEvent(t, s, 180, 12, advancedNewReset, false)
	insertAdvancedResetEvent(t, s, 240, 13, advancedNewReset, false)
	cycles, err := s.cycles(ctx, advancedTestAccount, 10)
	if err != nil || len(cycles) != 1 || cycles[0].EndPercent != 13 {
		t.Fatalf("known plan recovery created a reset: cycles=%#v err=%v", cycles, err)
	}
	anomalies, err := s.quotaRegimeAnomalies(ctx, cycles[0].ID)
	if err != nil || len(anomalies) != 1 {
		t.Fatalf("known plan recovery anomaly=%#v err=%v", anomalies, err)
	}
}

func TestAdvancedEarlyResetRejectsInsufficientEvidence(t *testing.T) {
	type reading struct {
		offset int64
		used   float64
		reset  int64
		failed bool
	}
	for _, tc := range []struct {
		name     string
		readings []reading
	}{
		{"only_two", []reading{{120, 2, advancedNewReset, false}, {7200, 9, advancedNewReset, false}}},
		{"too_fast", []reading{{120, 2, advancedNewReset, false}, {130, 9, advancedNewReset, false}, {140, 10, advancedNewReset, false}}},
		{"failed_middle", []reading{{120, 2, advancedNewReset, false}, {180, 9, advancedNewReset, true}, {240, 10, advancedNewReset, false}}},
		{"out_of_order", []reading{{120, 2, advancedNewReset, false}, {240, 9, advancedNewReset, false}, {180, 10, advancedNewReset, false}}},
		{"usage_falls_again", []reading{{120, 9, advancedNewReset, false}, {180, 2, advancedNewReset, false}, {240, 3, advancedNewReset, false}}},
		{"old_plan_rebounds", []reading{{120, 2, advancedNewReset, false}, {180, 9, advancedNewReset, false}, {240, 81, advancedOldReset, false}}},
		{"new_plan_rebounds", []reading{{120, 2, advancedNewReset, false}, {180, 9, advancedNewReset, false}, {240, 81, advancedNewReset, false}}},
		{"small_drop", []reading{{120, 50, advancedNewReset, false}, {180, 51, advancedNewReset, false}, {240, 52, advancedNewReset, false}}},
		{"schedule_keeps_changing", []reading{{120, 2, advancedNewReset, false}, {180, 9, advancedNewReset + 60, false}, {240, 10, advancedNewReset + 120, false}}},
		{"existing_window_correction", []reading{{120, 2, advancedOldReset + 3600, false}, {180, 9, advancedOldReset + 3600, false}, {240, 10, advancedOldReset + 3600, false}}},
		{"same_schedule_above_five", []reading{{120, 2, advancedOldReset, false}, {180, 9, advancedOldReset, false}, {240, 10, advancedOldReset, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "evidence.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			insertAdvancedResetEvent(t, s, -3600, 80, advancedOldReset, false)
			for _, item := range tc.readings {
				insertAdvancedResetEvent(t, s, item.offset, item.used, item.reset, item.failed)
			}
			cycles, err := s.cycles(context.Background(), advancedTestAccount, 10)
			if err != nil || len(cycles) != 1 || cycles[0].Requests != int64(len(tc.readings)+1) {
				t.Fatalf("insufficient evidence created a cycle or lost requests: cycles=%#v err=%v", cycles, err)
			}
		})
	}
}

func TestAdvancedEarlyResetRecoveryRejectsFalseRollback(t *testing.T) {
	for _, scenario := range []string{"single_stale_response", "failed_response", "out_of_order", "real_refill_reaches_old_peak", "old_window_expired"} {
		t.Run(scenario, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "false-rollback.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			ctx := context.Background()
			insertAdvancedResetEvent(t, s, -3600, 80, advancedOldReset, false)
			insertAdvancedResetEvent(t, s, 120, 2, advancedNewReset, false)
			insertAdvancedResetEvent(t, s, 180, 9, advancedNewReset, false)
			insertAdvancedResetEvent(t, s, 240, 10, advancedNewReset, false)
			switch scenario {
			case "single_stale_response":
				insertAdvancedResetEvent(t, s, 300, 81, advancedOldReset, false)
				insertAdvancedResetEvent(t, s, 360, 11, advancedNewReset, false)
			case "failed_response":
				insertAdvancedResetEvent(t, s, 300, 81, advancedOldReset, true)
				insertAdvancedResetEvent(t, s, 360, 82, advancedOldReset, false)
			case "out_of_order":
				insertAdvancedResetEvent(t, s, 360, 81, advancedOldReset, false)
				insertAdvancedResetEvent(t, s, 300, 82, advancedOldReset, false)
			case "real_refill_reaches_old_peak":
				// The schedule can be corrected while genuinely refilled usage
				// stays low. Later growth is not evidence of undoing the refill.
				insertAdvancedResetEvent(t, s, 300, 11, advancedOldReset, false)
				insertAdvancedResetEvent(t, s, 360, 12, advancedOldReset, false)
				insertAdvancedResetEvent(t, s, 7200, 80, advancedOldReset, false)
				insertAdvancedResetEvent(t, s, 7260, 81, advancedOldReset, false)
			case "old_window_expired":
				insertAdvancedResetEvent(t, s, advancedOldReset-advancedResetAt+60, 81, advancedOldReset, false)
				insertAdvancedResetEvent(t, s, advancedOldReset-advancedResetAt+120, 82, advancedOldReset, false)
			}
			cycles, err := s.cycles(ctx, advancedTestAccount, 10)
			if err != nil || len(cycles) != 2 || cycles[1].CloseReason != "early_reset" {
				t.Fatalf("unconfirmed rollback erased a reset: cycles=%#v err=%v", cycles, err)
			}
			if scenario == "real_refill_reaches_old_peak" && cycles[0].EndPercent != 81 {
				t.Fatalf("rejected rollback blocked subsequent quota sampling: %#v", cycles[0])
			}
		})
	}
}

func TestAdvancedEarlyResetRecoveryIsAtomic(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "atomic.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	insertAdvancedResetEvent(t, s, -3600, 80, advancedOldReset, false)
	insertAdvancedResetEvent(t, s, 120, 2, advancedNewReset, false)
	insertAdvancedResetEvent(t, s, 180, 9, advancedNewReset, false)
	insertAdvancedResetEvent(t, s, 240, 10, advancedNewReset, false)
	insertAdvancedResetEvent(t, s, 300, 81, advancedOldReset, false)
	cycles, err := s.cycles(ctx, advancedTestAccount, 10)
	if err != nil || len(cycles) != 2 {
		t.Fatalf("cycles=%#v err=%v", cycles, err)
	}
	childID := cycles[0].ID
	if _, err = s.db.Exec(fmt.Sprintf(`CREATE TRIGGER fail_recovery BEFORE DELETE ON quota_cycles WHEN OLD.id=%d BEGIN SELECT RAISE(ABORT,'injected recovery failure'); END`, childID)); err != nil {
		t.Fatal(err)
	}
	used := 82.0
	e := event{Account: advancedTestAccount, RequestedAt: advancedResetAt + 360, ObservedAt: advancedResetAt + 360,
		UsedPercent: &used, ResetAt: advancedOldReset, WindowMinutes: 10080, PlanType: "pro", TotalTokens: 100, CostUSD: 1}
	if err = s.insertEvent(ctx, e, time.Minute); err == nil {
		t.Fatal("expected the injected transaction failure")
	}
	cycles, err = s.cycles(ctx, advancedTestAccount, 10)
	if err != nil || len(cycles) != 2 || cycles[0].ID != childID || cycles[0].Requests != 4 || cycles[1].Requests != 1 || cycles[1].EndedAt == 0 {
		t.Fatalf("failed recovery partially changed the ledger: cycles=%#v err=%v", cycles, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_recovery`); err != nil {
		t.Fatal(err)
	}
	if err = s.insertEvent(ctx, e, time.Minute); err != nil {
		t.Fatal(err)
	}
	cycles, err = s.cycles(ctx, advancedTestAccount, 10)
	if err != nil || len(cycles) != 1 || cycles[0].Requests != 6 || cycles[0].ActualTokens != 600 {
		t.Fatalf("retry did not restore the intact ledger: cycles=%#v err=%v", cycles, err)
	}
	anomalies, err := s.quotaRegimeAnomalies(ctx, cycles[0].ID)
	if err != nil || len(anomalies) != 1 || anomalies[0].BeforeUsedPercent != 80 || anomalies[0].RestoredUsedPercent != 81 {
		t.Fatalf("a single retained pre-change observation lost rollback evidence: anomalies=%#v err=%v", anomalies, err)
	}
}
