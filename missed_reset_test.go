package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Times and quota readings reproduce the 2026-08-28 weekly reset; account and
// token values are synthetic. Concurrent requests are replayed in ID order.
func TestEarlyResetAfterOverlappingWeeklySchedule(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "reset.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	const account = "synthetic-account"
	const oldReset = int64(1788272040) // 2026-09-01 22:14 CST
	const newReset = int64(1788452820) // 2026-09-04 00:27 CST
	rows := []struct {
		request, observed int64
		used              float64
		reset             int64
	}{
		{1787847995, 1787847997, 29, oldReset},
		{1787848004, 1787848006, 0, newReset},
		{1787848016, 1787848018, 0, newReset},
		{1787848027, 1787848030, 0, newReset},
		{1787848069, 1787848069, 0, newReset},
		{1787848070, 1787848070, 0, newReset},
	}
	for _, row := range rows {
		used := row.used
		if err := s.insertEvent(ctx, event{Account: account, RequestedAt: row.request, ObservedAt: row.observed,
			UsedPercent: &used, ResetAt: row.reset, WindowMinutes: 10080, PlanType: "pro", TotalTokens: 100}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 {
		t.Fatalf("expected confirmed reset to split cycle; got %#v", cycles)
	}
	if cycles[0].ResetAt != newReset || cycles[1].ResetAt != oldReset {
		t.Fatalf("wrong schedules: %#v", cycles)
	}
}

func TestMissedResetHistoricalRepairIsIdempotent(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	const oldReset = int64(1788272040)
	const first = int64(1787848004)
	const newReset = int64(1788452820)
	_, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,ended_at,reset_at,window_minutes,plan_type,close_reason) VALUES(1,'synthetic-account',?,?,?,?,'pro','early_reset')`, oldReset-7*86400, first+2*86400, newReset, 10080)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		at    int64
		used  float64
		reset int64
	}{{first - 10, 29, oldReset}, {first, 0, newReset}, {first + 30, 0, newReset}, {first + 65, 0, newReset}, {first + 3600, 29, newReset}, {first + 7200, 31, newReset}} {
		_, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,account,requested_at,observed_at,used_percent,reset_at,window_minutes,plan_type,total_tokens,cost_usd,quota_scope) VALUES(1,'synthetic-account',?,?,?,?,?,'pro',100,1,'main')`, row.at, row.at+2, row.used, row.reset, 10080)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		at    int64
		used  float64
		reset int64
	}{{first - 10, 29, oldReset}, {first + 3600, 29, newReset}, {first + 7200, 31, newReset}} {
		_, err = s.db.Exec(`INSERT INTO quota_samples(cycle_id,account,sampled_at,used_percent,reset_at,window_minutes) VALUES(1,'synthetic-account',?,?,?,10080)`, row.at, row.used, row.reset)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`DELETE FROM metadata WHERE key=?`, missedResetRepairKey); err != nil {
		t.Fatal(err)
	}
	boundaries, err := s.repairMissedAdvancedEarlyResets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(boundaries) != 1 || boundaries[0].At != first {
		t.Fatalf("boundaries=%#v", boundaries)
	}
	cycles, err := s.cycles(ctx, "synthetic-account", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 || cycles[1].PeakPercent != 29 || cycles[1].EndedAt != first || cycles[0].StartPercent != 0 || cycles[0].PeakPercent != 31 {
		t.Fatalf("cycles=%#v", cycles)
	}
	var newEvents, firstSamples int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE cycle_id=?`, cycles[0].ID).Scan(&newEvents); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples WHERE cycle_id=? AND sampled_at=? AND used_percent=0`, cycles[0].ID, first).Scan(&firstSamples); err != nil {
		t.Fatal(err)
	}
	if newEvents != 5 || firstSamples != 1 {
		t.Fatalf("new events=%d first samples=%d", newEvents, firstSamples)
	}
	again, err := s.repairMissedAdvancedEarlyResets(ctx)
	if err != nil || len(again) != 0 {
		t.Fatalf("second repair=%#v err=%v", again, err)
	}
	var cycleCount int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_cycles`).Scan(&cycleCount); err != nil {
		t.Fatal(err)
	}
	if cycleCount != 2 {
		t.Fatalf("cycle count after repeat=%d", cycleCount)
	}
}
