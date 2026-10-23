package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

const (
	overlapAccount  = "overlapping-reset-synthetic"
	overlapStarted  = int64(1792689000)
	overlapOldReset = int64(1793293800)
	overlapNewReset = int64(1793382060)
	overlapFirst    = int64(1792777252)
)

type overlapReading struct {
	requested, observed int64
	used                float64
	reset               int64
}

// Production request/observation times, with synthetic account and usage.
func overlappingResetReadings() []overlapReading {
	return []overlapReading{
		{overlapStarted, overlapStarted, 0, overlapOldReset},
		{1792777238, 1792777265, 65, overlapOldReset},
		{1792777205, 1792777217, 65, overlapOldReset},
		{1792777266, 1792777272, 0, overlapNewReset},
		{overlapFirst, 1792777264, 0, overlapNewReset},
		{1792777269, 1792777279, 0, overlapNewReset},
		{1792777284, 1792777290, 0, overlapNewReset},
		{1792777304, 1792777307, 0, overlapNewReset},
		{1792777322, 1792777327, 0, overlapNewReset},
		{1792777377, 1792777386, 1, overlapNewReset},
	}
}

func assertOverlapCycles(t *testing.T, s *store) {
	t.Helper()
	ctx := context.Background()
	cycles, err := s.cycles(ctx, overlapAccount, 10)
	if err != nil || len(cycles) != 2 {
		t.Fatalf("expected two cycles: cycles=%#v err=%v", cycles, err)
	}
	current, previous := cycles[0], cycles[1]
	if current.StartedAt != overlapFirst || current.ResetAt != overlapNewReset || current.StartPercent != 0 ||
		current.ActualTokens != 700 || current.ActualCostUSD != 7 || current.Requests != 7 {
		t.Fatalf("new cycle carries old usage or misses overlapping requests: %#v", current)
	}
	if previous.StartedAt != overlapStarted || previous.EndedAt != overlapFirst || previous.ResetAt != overlapOldReset ||
		previous.PeakPercent != 65 || previous.ActualTokens != 300 || previous.ActualCostUSD != 3 ||
		previous.Requests != 3 || previous.CloseReason != "early_reset" {
		t.Fatalf("old cycle baseline changed: %#v", previous)
	}
	points, _, err := s.pointsForCycle(ctx, overlapAccount, current.ID, 100)
	if err != nil || len(points) < 2 || points[0].Time != overlapFirst || points[0].WindowTokens != 100 ||
		points[len(points)-1].WindowTokens != 700 {
		t.Fatalf("new sample aggregates are not cycle-local: points=%#v err=%v", points, err)
	}
	confirmed, _, err := s.historicalEarlyResetConfirmed(ctx, previous, current)
	if err != nil || !confirmed {
		t.Fatalf("historical confirmation rejected overlapping reset: confirmed=%v err=%v", confirmed, err)
	}
	month, err := s.monthly(ctx, overlapAccount, "2026-10")
	if err != nil || month.ActualTokens != 1000 || month.ActualCostUSD != 10 || month.Requests != 10 || month.EarlyResetCount != 1 {
		t.Fatalf("monthly totals or reset count changed: month=%#v err=%v", month, err)
	}
}

func TestAdvancedResetOverlappingHeadersKeepPendingBaseline(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "live"
		if restart {
			name = "restart_while_pending"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "overlap.sqlite")
			s, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.close() }()
			for i, r := range overlappingResetReadings() {
				if err := s.insertEvent(context.Background(), event{
					Account: overlapAccount, RequestedAt: r.requested, ObservedAt: r.observed,
					UsedPercent: &r.used, ResetAt: r.reset, WindowMinutes: 10080, PlanType: "pro",
					TotalTokens: 100, CostUSD: 1,
				}, time.Minute); err != nil {
					t.Fatal(err)
				}
				if i >= 3 && i <= 7 {
					cycles, err := s.cycles(context.Background(), overlapAccount, 10)
					if err != nil || len(cycles) != 1 || cycles[0].ResetAt != overlapOldReset || cycles[0].PeakPercent != 65 {
						t.Fatalf("pending overlap discarded old baseline at reading %d: cycles=%#v err=%v", i, cycles, err)
					}
				}
				if restart && i == 5 {
					if err = s.close(); err != nil {
						t.Fatal(err)
					}
					s, err = openStore(path)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			assertOverlapCycles(t, s)
		})
	}
}

func seedMissedOverlap(t *testing.T, s *store) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes,plan_type)
VALUES(1,?,?,?,10080,'pro')`, overlapAccount, overlapStarted, overlapNewReset)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range overlappingResetReadings() {
		if _, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,account,requested_at,observed_at,used_percent,reset_at,window_minutes,plan_type,total_tokens,cost_usd)
VALUES(1,?,?,?,?,?,10080,'pro',100,1)`, overlapAccount, r.requested, r.observed, r.used, r.reset); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []overlapReading{overlappingResetReadings()[0], overlappingResetReadings()[1],
		overlappingResetReadings()[5], overlappingResetReadings()[9]} {
		if _, err = s.db.Exec(`INSERT INTO quota_samples(cycle_id,account,sampled_at,used_percent,reset_at,window_minutes,window_tokens,window_cost_usd,requests)
VALUES(1,?,?,?,?,10080,1000,10,10)`, overlapAccount, r.requested, r.used, r.reset); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`INSERT INTO metadata(key,value) VALUES('missed_advanced_early_resets_v1','complete')
ON CONFLICT(key) DO UPDATE SET value='complete'; DELETE FROM metadata WHERE key=?`, missedResetRepairKey); err != nil {
		t.Fatal(err)
	}
}

func TestMissedOverlapRepairUpgradesCompletedV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missed-overlap.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	seedMissedOverlap(t, s)
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	assertOverlapCycles(t, s)
	again, err := s.repairMissedAdvancedEarlyResets(context.Background())
	if err != nil || len(again) != 0 {
		t.Fatalf("repair is not idempotent: boundaries=%#v err=%v", again, err)
	}
	var marker string
	if err = s.db.QueryRow(`SELECT value FROM metadata WHERE key=?`, missedResetRepairKey).Scan(&marker); err != nil || marker != "complete" {
		t.Fatalf("v2 marker=%q err=%v", marker, err)
	}
}

func TestMissedOverlapRepairIsAtomic(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "atomic-overlap.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	seedMissedOverlap(t, s)
	if _, err = s.db.Exec(`CREATE TRIGGER reject_overlap_split BEFORE UPDATE OF cycle_id ON usage_events
BEGIN SELECT RAISE(ABORT,'injected split failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.repairMissedAdvancedEarlyResets(context.Background()); err == nil {
		t.Fatal("expected injected split failure")
	}
	var cycles, markers, events int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_cycles`).Scan(&cycles); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM metadata WHERE key=?`, missedResetRepairKey).Scan(&markers); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE cycle_id=1`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if cycles != 1 || markers != 0 || events != 10 {
		t.Fatalf("failed split partially committed: cycles=%d markers=%d events=%d", cycles, markers, events)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_overlap_split`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.repairMissedAdvancedEarlyResets(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertOverlapCycles(t, s)
}

func TestMissedOverlapRepairRejectsNonResetEvidence(t *testing.T) {
	for _, scenario := range []string{"known_plan", "existing_boundary", "original_plan_recovers", "insufficient_span", "failed_confirmation"} {
		t.Run(scenario, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "rejected-overlap.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			seedMissedOverlap(t, s)
			switch scenario {
			case "known_plan":
				_, err = s.db.Exec(`INSERT INTO quota_samples(cycle_id,account,sampled_at,used_percent,reset_at,window_minutes)
VALUES(1,?,?,0,?,10080)`, overlapAccount, overlapStarted+60, overlapNewReset)
			case "existing_boundary":
				_, err = s.db.Exec(`UPDATE quota_cycles SET started_at=? WHERE id=1`, overlapFirst-60)
			case "original_plan_recovers":
				_, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,account,requested_at,observed_at,used_percent,reset_at,window_minutes,plan_type)
VALUES(1,?,?,?,65,?,10080,'pro')`, overlapAccount, overlapFirst+3600, overlapFirst+3602, overlapOldReset)
			case "insufficient_span":
				_, err = s.db.Exec(`DELETE FROM usage_events WHERE observed_at>=?`, overlapFirst+40)
			case "failed_confirmation":
				_, err = s.db.Exec(`UPDATE usage_events SET failed=1 WHERE observed_at=?`, 1792777307)
			}
			if err != nil {
				t.Fatal(err)
			}
			boundaries, err := s.repairMissedAdvancedEarlyResets(context.Background())
			if err != nil || len(boundaries) != 0 {
				t.Fatalf("non-reset evidence split a cycle: boundaries=%#v err=%v", boundaries, err)
			}
		})
	}
}
