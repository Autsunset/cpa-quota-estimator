package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoricalWindowsAndPoints(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "history.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	const account = "codex@example.com"

	insertSample := func(sampledAt, resetAt, windowMinutes int64, used float64, tokens int64, cost float64, requests int64) {
		t.Helper()
		_, err := s.db.ExecContext(ctx, "INSERT INTO quota_samples(sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,window_cost_usd,requests) VALUES(?,?,?,?,?,?,?,?,?)",
			sampledAt, account, used, resetAt, windowMinutes, "pro", tokens, cost, requests)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertEvent := func(requestedAt, tokens int64, cost float64) {
		t.Helper()
		_, err := s.db.ExecContext(ctx, "INSERT INTO usage_events(requested_at,account,total_tokens,cost_usd) VALUES(?,?,?,?)",
			requestedAt, account, tokens, cost)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Window 1 runs from 100 to 700. The event at exactly 700 belongs to
	// window 2 and must not be included in the historical final aggregate.
	insertSample(200, 700, 10, 10, 10, 1, 1)
	insertSample(650, 700, 10, 60, 20, 2, 2)
	insertEvent(300, 10, 1)
	insertEvent(680, 20, 2)
	insertEvent(700, 100, 10)

	insertSample(750, 1300, 10, 5, 100, 10, 1)
	insertSample(900, 1300, 10, 15, 120, 12, 2)

	windows, err := s.windows(ctx, account, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || windows[0].ResetAt != 1300 || windows[1].ResetAt != 700 {
		t.Fatalf("unexpected windows: %#v", windows)
	}
	if windows[1].WindowStart != 100 || windows[1].EndPercent != 60 {
		t.Fatalf("unexpected historical window: %#v", windows[1])
	}

	points, plan, err := s.pointsForWindow(ctx, account, 700, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if plan != "pro" || len(points) != 3 {
		t.Fatalf("unexpected historical points: plan=%q points=%#v", plan, points)
	}
	last := points[len(points)-1]
	if last.Time != 680 || last.WindowTokens != 30 || last.WindowCostUSD != 3 || last.Requests != 2 {
		t.Fatalf("unexpected final aggregate: %#v", last)
	}
}

func TestSelectHistoricalWindow(t *testing.T) {
	windows := []quotaWindow{{ResetAt: 200}, {ResetAt: 100}}
	selected, current := selectWindow(windows, "100")
	if current || selected.ResetAt != 100 {
		t.Fatalf("selected=%#v current=%v", selected, current)
	}
	selected, current = selectWindow(windows, "invalid")
	if !current || selected.ResetAt != 200 {
		t.Fatalf("fallback selected=%#v current=%v", selected, current)
	}
}

func TestCycleChangesWhenResetScheduleChanges(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "cycles.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "cycle-account"
	insert := func(at int64, used float64, resetAt int64, tokens int64) {
		t.Helper()
		e := event{RequestedAt: at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: tokens, UsedPercent: &used, ResetAt: resetAt, WindowMinutes: 10, PlanType: "pro"}
		if err := s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	insert(100, 10, 700, 100)
	insert(200, 20, 700, 200)
	if err := s.insertEvent(ctx, event{RequestedAt: 705, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 50}, time.Minute); err != nil {
		t.Fatal(err)
	}
	insert(710, 0, 1300, 300)

	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 {
		t.Fatalf("cycles = %#v", cycles)
	}
	current, previous := cycles[0], cycles[1]
	if !current.Current || current.StartedAt != 700 || current.ResetAt != 1300 {
		t.Fatalf("current cycle = %#v", current)
	}
	if previous.EndedAt != 700 || previous.CloseReason != "scheduled_reset" || previous.ActualTokens != 300 {
		t.Fatalf("previous cycle = %#v", previous)
	}
	if current.ActualTokens != 350 {
		t.Fatalf("current tokens = %d", current.ActualTokens)
	}
}

func TestFirstQuotaCycleAdoptsEarlierUnassignedRequests(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "adopt-unassigned.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "adopt-unassigned-account"
	if err = s.insertEvent(ctx, event{RequestedAt: 150, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 100}, time.Minute); err != nil {
		t.Fatal(err)
	}
	used := 10.0
	if err = s.insertEvent(ctx, event{RequestedAt: 200, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 200, UsedPercent: &used, ResetAt: 700, WindowMinutes: 10, PlanType: "pro"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].ActualTokens != 300 || cycles[0].Requests != 2 {
		t.Fatalf("cycles = %#v", cycles)
	}
	points, _, err := s.pointsForCycle(ctx, account, cycles[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].WindowTokens != 300 || points[0].Requests != 2 {
		t.Fatalf("points = %#v", points)
	}
}

func TestCycleConfirmsEarlyResetWithSameResetAt(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "early-reset.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "early-reset-account"
	insert := func(at int64, used float64, tokens int64) {
		t.Helper()
		e := event{RequestedAt: at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: tokens, UsedPercent: &used, ResetAt: 700, WindowMinutes: 10, PlanType: "pro"}
		if err := s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	insert(100, 40, 100)
	insert(110, 0, 200)
	insert(110, 1, 300)

	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 {
		t.Fatalf("cycles = %#v", cycles)
	}
	current, previous := cycles[0], cycles[1]
	if current.StartedAt != 110 || current.ResetAt != 700 || current.ActualTokens != 500 {
		t.Fatalf("current cycle = %#v", current)
	}
	if previous.EndedAt != 110 || previous.CloseReason != "early_reset" || previous.ActualTokens != 100 {
		t.Fatalf("previous cycle = %#v", previous)
	}
	points, _, err := s.pointsForCycle(ctx, account, current.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].UsedPercent != 0 || points[1].UsedPercent != 1 || points[1].WindowTokens != 500 {
		t.Fatalf("new cycle points = %#v", points)
	}
}

func TestCycleRejectsSingleStaleLowerSample(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "stale-reset.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "stale-account"
	for _, item := range []struct {
		at   int64
		used float64
	}{{100, 40}, {110, 0}, {120, 41}} {
		e := event{RequestedAt: item.at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 100, UsedPercent: &item.used, ResetAt: 700, WindowMinutes: 10, PlanType: "pro"}
		if err := s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].PeakPercent != 41 || cycles[0].ActualTokens != 300 {
		t.Fatalf("cycles = %#v", cycles)
	}
}

func TestCycleRejectsRepeatedPartialDrop(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "partial-drop.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "partial-drop-account"
	for _, item := range []struct {
		at   int64
		used float64
	}{{100, 40}, {110, 20}, {120, 21}} {
		e := event{RequestedAt: item.at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 100, UsedPercent: &item.used, ResetAt: 700, WindowMinutes: 10, PlanType: "pro"}
		if err := s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].PeakPercent != 40 || cycles[0].ActualTokens != 300 {
		t.Fatalf("cycles = %#v", cycles)
	}
}

func TestCycleKeepsScheduleCorrectionInCurrentCycle(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "schedule-correction.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "schedule-correction-account"
	for _, item := range []struct {
		at      int64
		used    float64
		resetAt int64
	}{{100, 40, 700}, {200, 41, 800}} {
		e := event{RequestedAt: item.at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 100, UsedPercent: &item.used, ResetAt: item.resetAt, WindowMinutes: 10, PlanType: "pro"}
		if err := s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].ResetAt != 800 || cycles[0].PeakPercent != 41 || cycles[0].ActualTokens != 200 {
		t.Fatalf("cycles = %#v", cycles)
	}
	points, _, err := s.pointsForCycle(ctx, account, cycles[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if estimate := estimateCapacity(points); !estimate.Available {
		t.Fatalf("estimate unavailable after schedule correction: points=%#v", points)
	}
}

func TestSelectCycleDistinguishesSameResetAt(t *testing.T) {
	cycles := []quotaCycle{{ID: 2, ResetAt: 700, Current: true}, {ID: 1, ResetAt: 700}}
	selected, current := selectCycle(cycles, "1", "")
	if current || selected.ID != 1 {
		t.Fatalf("selected=%#v current=%v", selected, current)
	}
	selected, current = selectCycle(cycles, "", "700")
	if !current || selected.ID != 2 {
		t.Fatalf("reset fallback selected=%#v current=%v", selected, current)
	}
}

func TestMonthlyCycleJSONIncludesCycleFields(t *testing.T) {
	data, err := json.Marshal(monthlyCycle{quotaCycle: quotaCycle{ID: 7, StartedAt: 100}, MonthTokens: 42})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range [][]byte{[]byte(`"id":7`), []byte(`"started_at":100`), []byte(`"month_tokens":42`)} {
		if !bytes.Contains(data, field) {
			t.Fatalf("missing %s in %s", field, data)
		}
	}
}

func TestMonthRangeRejectsInvalidMonth(t *testing.T) {
	if _, _, _, err := monthRange("2026-13"); err == nil {
		t.Fatal("expected invalid month error")
	}
}

func TestLegacyDatabaseBackfillsQuotaCycles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account := "legacy-account"
	for _, row := range []struct {
		at, resetAt, tokens int64
		used                float64
	}{{200, 700, 100, 10}, {650, 700, 200, 60}, {750, 1300, 300, 5}} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,account,total_tokens,used_percent,reset_at,window_minutes,plan_type) VALUES(?,?,?,?,?,?,?)`, row.at, account, row.tokens, row.used, row.resetAt, 10, "pro"); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO quota_samples(sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,requests) VALUES(?,?,?,?,?,?,?,?)`, row.at, account, row.used, row.resetAt, 10, "pro", row.tokens, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Exec(`DROP INDEX idx_usage_cycle_time; DROP INDEX idx_quota_cycle_time; DROP TABLE quota_cycles; ALTER TABLE usage_events DROP COLUMN cycle_id; ALTER TABLE quota_samples DROP COLUMN cycle_id;`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 || cycles[0].ResetAt != 1300 || cycles[1].ResetAt != 700 {
		t.Fatalf("cycles = %#v", cycles)
	}
	if cycles[1].EndedAt != 700 || cycles[1].CloseReason != "observed_reset" || cycles[1].ActualTokens != 300 || cycles[0].ActualTokens != 300 {
		t.Fatalf("backfilled cycles = %#v", cycles)
	}
}

func TestMonthlySummaryAcrossCycles(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "monthly.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	location := shanghaiLocation()
	stamp := func(day int) int64 { return time.Date(2026, 1, day, 12, 0, 0, 0, location).Unix() }
	account := "monthly-account"
	result, err := s.db.ExecContext(ctx, `INSERT INTO quota_cycles(account,started_at,ended_at,reset_at,window_minutes,plan_type,close_reason,first_sample_at,last_sample_at,start_used_percent,end_used_percent,peak_used_percent) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, account, stamp(1), stamp(8), stamp(8), 10080, "pro", "scheduled_reset", stamp(1), stamp(7), 0, 80, 80)
	if err != nil {
		t.Fatal(err)
	}
	cycle1, _ := result.LastInsertId()
	result, err = s.db.ExecContext(ctx, `INSERT INTO quota_cycles(account,started_at,reset_at,window_minutes,plan_type,first_sample_at,last_sample_at,start_used_percent,end_used_percent,peak_used_percent) VALUES(?,?,?,?,?,?,?,?,?,?)`, account, stamp(8), stamp(15), 10080, "pro", stamp(8), stamp(14), 0, 50, 50)
	if err != nil {
		t.Fatal(err)
	}
	cycle2, _ := result.LastInsertId()
	for _, row := range []struct {
		cycle int64
		at    int64
		used  float64
		token int64
		cost  float64
	}{{cycle1, stamp(1), 0, 10, 1}, {cycle1, stamp(7), 80, 90, 9}, {cycle2, stamp(8), 0, 20, 2}, {cycle2, stamp(14), 50, 80, 8}} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(cycle_id,requested_at,account,total_tokens,cost_usd) VALUES(?,?,?,?,?)`, row.cycle, row.at, account, row.token, row.cost); err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,window_cost_usd,requests) VALUES(?,?,?,?,?,?,?,?,?,?)`, row.cycle, row.at, account, row.used, stamp(15), 10080, "pro", row.token, row.cost, 1); err != nil {
			t.Fatal(err)
		}
	}

	summary, err := s.monthly(ctx, account, "2026-01")
	if err != nil {
		t.Fatal(err)
	}
	if summary.CycleCount != 2 || summary.ResetCount != 1 || summary.EarlyResetCount != 0 || summary.ActualTokens != 200 || summary.Requests != 4 {
		t.Fatalf("monthly summary = %#v", summary)
	}
	if summary.ConsumedQuotaPercent != 130 || summary.ConsumedQuotaEquivalent != 1.3 || summary.UnusedQuotaAtReset != 20 {
		t.Fatalf("monthly quota = %#v", summary)
	}
	if summary.AllocatedCycleCount != 2 || summary.EstimatedCycleCount != 2 || summary.EstimatedTokens <= 0 || len(summary.Cycles) != 2 {
		t.Fatalf("monthly capacity = %#v", summary)
	}
}

func TestPointsForRangeSpansCyclesInTimeOrder(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "range.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "range-account"

	result, err := s.db.ExecContext(ctx, `INSERT INTO quota_cycles(account,started_at,ended_at,reset_at,window_minutes,plan_type,close_reason) VALUES(?,?,?,?,?,?,?)`, account, 100, 300, 300, 10, "pro", "scheduled_reset")
	if err != nil {
		t.Fatal(err)
	}
	cycle1, _ := result.LastInsertId()
	result, err = s.db.ExecContext(ctx, `INSERT INTO quota_cycles(account,started_at,reset_at,window_minutes,plan_type) VALUES(?,?,?,?,?)`, account, 300, 900, 10, "pro")
	if err != nil {
		t.Fatal(err)
	}
	cycle2, _ := result.LastInsertId()

	for _, row := range []struct {
		cycle int64
		at    int64
		used  float64
		reset int64
	}{{cycle1, 150, 10, 300}, {cycle1, 250, 20, 300}, {cycle2, 350, 5, 900}, {cycle2, 450, 15, 900}} {
		_, err = s.db.ExecContext(ctx, `INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,window_cost_usd,requests) VALUES(?,?,?,?,?,?,?,?,?,?)`, row.cycle, row.at, account, row.used, row.reset, 10, "pro", row.at, float64(row.at)/10, 1)
		if err != nil {
			t.Fatal(err)
		}
	}

	points, cycles, err := s.pointsForRange(ctx, account, 200, 400, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 || cycles[0].ID != cycle1 || cycles[1].ID != cycle2 {
		t.Fatalf("cycles = %#v", cycles)
	}
	if len(points) != 2 || points[0].CycleID != cycle1 || points[0].Time != 250 || points[1].CycleID != cycle2 || points[1].Time != 350 {
		t.Fatalf("points = %#v", points)
	}
}

func TestMonthlySummaryIncludesEarlyResetAcrossMonthBoundary(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "monthly-early-reset.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	location := shanghaiLocation()
	stamp := func(month time.Month, day, hour, minute int) int64 {
		return time.Date(2026, month, day, hour, minute, 0, 0, location).Unix()
	}
	account := "monthly-early-reset-account"
	resetAt := stamp(time.February, 7, 0, 0)
	for _, item := range []struct {
		at     int64
		used   float64
		tokens int64
	}{{stamp(time.January, 31, 12, 0), 40, 100}, {stamp(time.February, 1, 1, 0), 0, 200}, {stamp(time.February, 1, 1, 1), 1, 300}} {
		e := event{RequestedAt: item.at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: item.tokens, UsedPercent: &item.used, ResetAt: resetAt, WindowMinutes: 10080, PlanType: "pro"}
		if err = s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := s.monthly(ctx, account, "2026-02")
	if err != nil {
		t.Fatal(err)
	}
	if summary.CycleCount != 2 || summary.ResetCount != 1 || summary.EarlyResetCount != 1 {
		t.Fatalf("monthly cycles = %#v", summary)
	}
	if summary.ActualTokens != 500 || summary.Requests != 2 || summary.ConsumedQuotaPercent != 1 || !summary.QuotaCoverageComplete {
		t.Fatalf("monthly usage = %#v", summary)
	}
	if summary.UnusedQuotaAtReset != 60 {
		t.Fatalf("unused quota = %#v", summary)
	}
}

func TestMonthlyQuotaEquivalentMarksMissingBaselinePartial(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "monthly-partial.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	location := shanghaiLocation()
	startedAt := time.Date(2026, time.January, 20, 0, 0, 0, 0, location).Unix()
	sampledAt := time.Date(2026, time.February, 10, 0, 0, 0, 0, location).Unix()
	account := "monthly-partial-account"
	result, err := s.db.ExecContext(ctx, `INSERT INTO quota_cycles(account,started_at,reset_at,window_minutes,plan_type,first_sample_at,last_sample_at,start_used_percent,end_used_percent,peak_used_percent) VALUES(?,?,?,?,?,?,?,?,?,?)`, account, startedAt, sampledAt+86400, 10080, "pro", sampledAt, sampledAt, 50, 50, 50)
	if err != nil {
		t.Fatal(err)
	}
	cycleID, _ := result.LastInsertId()
	if _, err = s.db.ExecContext(ctx, `INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,plan_type) VALUES(?,?,?,?,?,?,?)`, cycleID, sampledAt, account, 50, sampledAt+86400, 10080, "pro"); err != nil {
		t.Fatal(err)
	}
	summary, err := s.monthly(ctx, account, "2026-02")
	if err != nil {
		t.Fatal(err)
	}
	if summary.QuotaCoverageComplete || summary.ConsumedQuotaPercent != 0 {
		t.Fatalf("monthly summary = %#v", summary)
	}
}
