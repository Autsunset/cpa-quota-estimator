package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRepairFalseEarlyResetChainPreservesUsage(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "repair-chain.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "repair-chain-account"
	base := time.Date(2026, time.August, 18, 13, 0, 0, 0, shanghaiLocation()).Unix()
	resetAt := base + 15*60

	cycle1 := insertRepairCycle(t, s, account, base, base+100, resetAt, "early_reset", 80, 81, 81)
	cycle2 := insertRepairCycle(t, s, account, base+100, base+200, resetAt, "early_reset", 0, 82, 82)
	cycle3 := insertRepairCycle(t, s, account, base+200, 0, resetAt, "", 0, 84, 84)
	for _, item := range []struct {
		cycle int64
		at    int64
		used  float64
		token int64
	}{
		{cycle1, base, 80, 100},
		{cycle1, base + 90, 81, 200},
		{cycle2, base + 100, 0, 300},
		{cycle2, base + 120, 81, 400},
		{cycle2, base + 150, 82, 500},
		{cycle3, base + 200, 0, 600},
		{cycle3, base + 220, 82, 700},
		{cycle3, base + 250, 84, 800},
	} {
		insertRepairEventAndSample(t, s, account, item.cycle, item.at, item.used, item.token, resetAt)
	}
	before, err := s.monthly(ctx, account, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if before.CycleCount != 3 || before.ResetCount != 2 || before.EarlyResetCount != 2 || before.ActualTokens != 3600 || before.ActualCostUSD != 36 || before.Requests != 8 || before.ConsumedQuotaPercent != 247 {
		t.Fatalf("monthly before repair = %#v", before)
	}

	preview, err := s.repairFalseEarlyResets(ctx, account, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Applied || preview.CandidateCount != 2 || preview.MergedCount != 0 {
		t.Fatalf("preview = %#v", preview)
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 3 {
		t.Fatalf("preview mutated cycles: %#v", cycles)
	}

	report, err := s.repairFalseEarlyResets(ctx, account, true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Applied || report.CandidateCount != 2 || report.MergedCount != 2 {
		t.Fatalf("report = %#v", report)
	}
	cycles, err = s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].ID != cycle3 || cycles[0].StartedAt != base || !cycles[0].Current || cycles[0].PeakPercent != 84 || cycles[0].ActualTokens != 3600 || cycles[0].ActualCostUSD != 36 || cycles[0].Requests != 8 {
		t.Fatalf("repaired cycles = %#v", cycles)
	}
	after, err := s.monthly(ctx, account, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if after.CycleCount != 1 || after.ResetCount != 0 || after.EarlyResetCount != 0 || after.ActualTokens != 3600 || after.ActualCostUSD != 36 || after.Requests != 8 || after.ConsumedQuotaPercent != 84 {
		t.Fatalf("monthly after repair = %#v", after)
	}
	var eventCount, lowSampleCount int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events WHERE account=?`, account).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_samples WHERE cycle_id=? AND used_percent<=?`, cycle3, resetLowPercent).Scan(&lowSampleCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 8 || lowSampleCount != 0 {
		t.Fatalf("events=%d low samples=%d", eventCount, lowSampleCount)
	}
	points, _, err := s.pointsForCycle(ctx, account, cycle3, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := points[len(points)-1]
	if last.WindowTokens != 3600 || last.WindowCostUSD != 36 || last.Requests != 8 {
		t.Fatalf("last repaired point = %#v", last)
	}
}

func TestRepairSparkQuotaResetChainPreservesActualUsage(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "repair-spark-chain.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "repair-spark-chain-account"
	base := time.Date(2026, time.August, 18, 13, 0, 0, 0, shanghaiLocation()).Unix()
	mainReset := base + 15*60
	sparkReset := mainReset + 59*60

	cycle1 := insertRepairCycle(t, s, account, base, base+100, mainReset, "early_reset", 80, 81, 81)
	cycle2 := insertRepairCycle(t, s, account, base+100, base+200, mainReset, "early_reset", 0, 82, 82)
	cycle3 := insertRepairCycle(t, s, account, base+200, 0, mainReset, "", 0, 84, 84)
	insertRepairScopedEventAndSample(t, s, account, cycle1, base, 80, 100, mainReset, mainQuotaScope)
	insertRepairScopedEventAndSample(t, s, account, cycle1, base+90, 81, 200, mainReset, mainQuotaScope)
	insertRepairScopedEventAndSample(t, s, account, cycle2, base+100, 0, 300, sparkReset, sparkQuotaScope)
	insertRepairScopedEventAndSample(t, s, account, cycle2, base+110, 81, 400, mainReset, mainQuotaScope)
	insertRepairScopedEventAndSample(t, s, account, cycle2, base+150, 82, 500, mainReset, mainQuotaScope)
	insertRepairScopedEventAndSample(t, s, account, cycle3, base+200, 0, 600, sparkReset, sparkQuotaScope)
	insertRepairScopedEventAndSample(t, s, account, cycle3, base+210, 82, 700, mainReset, mainQuotaScope)
	insertRepairScopedEventAndSample(t, s, account, cycle3, base+250, 84, 800, mainReset, mainQuotaScope)

	before, err := s.monthly(ctx, account, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	beforeSpark, err := s.monthlyQuotaScope(ctx, account, sparkQuotaScope, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.repairFalseEarlyResets(ctx, account, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.CandidateCount != 2 || !preview.Candidates[0].SeparateQuota || !preview.Candidates[1].SeparateQuota {
		t.Fatalf("preview = %#v", preview)
	}
	report, err := s.repairFalseEarlyResets(ctx, account, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.MergedCount != 2 {
		t.Fatalf("report = %#v", report)
	}

	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].ID != cycle3 || cycles[0].ActualTokens != 2700 || cycles[0].ActualCostUSD != 27 || cycles[0].Requests != 6 || cycles[0].PeakPercent != 84 {
		t.Fatalf("cycles = %#v", cycles)
	}
	after, err := s.monthly(ctx, account, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	afterSpark, err := s.monthlyQuotaScope(ctx, account, sparkQuotaScope, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if before.ActualTokens != 2700 || before.ActualCostUSD != 27 || before.Requests != 6 || after.ActualTokens != before.ActualTokens || after.ActualCostUSD != before.ActualCostUSD || after.Requests != before.Requests || after.CycleCount != 1 || after.ResetCount != 0 || after.ConsumedQuotaPercent != 84 {
		t.Fatalf("before=%#v after=%#v", before, after)
	}
	if beforeSpark.ActualTokens != 900 || beforeSpark.ActualCostUSD != 9 || beforeSpark.Requests != 2 || afterSpark.ActualTokens != beforeSpark.ActualTokens || afterSpark.ActualCostUSD != beforeSpark.ActualCostUSD || afterSpark.Requests != beforeSpark.Requests {
		t.Fatalf("Spark before=%#v after=%#v", beforeSpark, afterSpark)
	}
	var sparkRows, sparkAssigned, sparkSamples int64
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN cycle_id<>0 THEN 1 ELSE 0 END),0) FROM usage_events WHERE account=? AND quota_scope=?`, account, sparkQuotaScope).Scan(&sparkRows, &sparkAssigned); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_samples WHERE account=? AND reset_at=?`, account, sparkReset).Scan(&sparkSamples); err != nil {
		t.Fatal(err)
	}
	if sparkRows != 2 || sparkAssigned != 0 || sparkSamples != 0 {
		t.Fatalf("spark rows=%d assigned=%d samples=%d", sparkRows, sparkAssigned, sparkSamples)
	}
	points, _, err := s.pointsForCycle(ctx, account, cycle3, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := points[len(points)-1]
	if last.WindowTokens != 2700 || last.WindowCostUSD != 27 || last.Requests != 6 {
		t.Fatalf("last point = %#v", last)
	}
}

func TestRepairPreservesIsolatedUnconfirmedEarlyReset(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "repair-isolated.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "repair-isolated-account"

	cycle1 := insertRepairCycle(t, s, account, 100, 200, 1000, "early_reset", 79, 80, 80)
	cycle2 := insertRepairCycle(t, s, account, 200, 0, 1000, "", 0, 81, 81)
	insertRepairEventAndSample(t, s, account, cycle1, 100, 79, 100, 1000)
	insertRepairEventAndSample(t, s, account, cycle1, 190, 80, 100, 1000)
	insertRepairEventAndSample(t, s, account, cycle2, 200, 0, 100, 1000)
	insertRepairEventAndSample(t, s, account, cycle2, 220, 80, 100, 1000)
	insertRepairEventAndSample(t, s, account, cycle2, 250, 81, 100, 1000)

	report, err := s.repairFalseEarlyResets(ctx, account, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.CandidateCount != 0 || report.MergedCount != 0 {
		t.Fatalf("isolated reset selected for repair: %#v", report)
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 || cycles[1].ID != cycle1 || cycles[0].ID != cycle2 {
		t.Fatalf("cycles = %#v", cycles)
	}
}

func TestRepairPreservesConfirmedEarlyReset(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "repair-confirmed.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "repair-confirmed-account"

	cycle1 := insertRepairCycle(t, s, account, 100, 200, 1000, "early_reset", 79, 80, 80)
	cycle2 := insertRepairCycle(t, s, account, 200, 0, 1000, "", 0, 2, 2)
	insertRepairEventAndSample(t, s, account, cycle1, 100, 79, 100, 1000)
	insertRepairEventAndSample(t, s, account, cycle1, 190, 80, 100, 1000)
	insertRepairEventAndSample(t, s, account, cycle2, 200, 0, 100, 1000)
	insertRepairEventAndSample(t, s, account, cycle2, 230, 1, 100, 1000)
	insertRepairEventAndSample(t, s, account, cycle2, 260, 2, 100, 1000)

	report, err := s.repairFalseEarlyResets(ctx, account, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.CandidateCount != 0 || report.MergedCount != 0 {
		t.Fatalf("confirmed reset selected for repair: %#v", report)
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 || cycles[1].ID != cycle1 || cycles[0].ID != cycle2 {
		t.Fatalf("cycles = %#v", cycles)
	}
}

func insertRepairCycle(t *testing.T, s *store, account string, startedAt, endedAt, resetAt int64, closeReason string, startUsed, endUsed, peak float64) int64 {
	t.Helper()
	result, err := s.db.Exec(`INSERT INTO quota_cycles(account,started_at,ended_at,reset_at,window_minutes,plan_type,close_reason,first_sample_at,last_sample_at,start_used_percent,end_used_percent,peak_used_percent)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, account, startedAt, endedAt, resetAt, 15, "pro", closeReason, startedAt, max(startedAt, endedAt-10), startUsed, endUsed, peak)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertRepairEventAndSample(t *testing.T, s *store, account string, cycleID, at int64, used float64, tokens, resetAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO usage_events(cycle_id,requested_at,observed_at,account,provider,model,total_tokens,cost_usd,used_percent,reset_at,window_minutes,plan_type)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, cycleID, at, at, account, "openai", "gpt", tokens, float64(tokens)/100, used, resetAt, 15, "pro"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,window_cost_usd,requests)
VALUES(?,?,?,?,?,?,?,?,?,?)`, cycleID, at, account, used, resetAt, 15, "pro", tokens, float64(tokens)/100, 1); err != nil {
		t.Fatal(err)
	}
}

func insertRepairScopedEventAndSample(t *testing.T, s *store, account string, cycleID, at int64, used float64, tokens, resetAt int64, scope string) {
	t.Helper()
	model := "gpt-5.6-sol"
	if scope == sparkQuotaScope {
		model = "gpt-5.3-codex-spark"
	}
	if _, err := s.db.Exec(`INSERT INTO usage_events(cycle_id,requested_at,observed_at,account,provider,model,total_tokens,cost_usd,used_percent,reset_at,window_minutes,plan_type,quota_scope)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, cycleID, at, at, account, "openai", model, tokens, float64(tokens)/100, used, resetAt, 15, "pro", scope); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,window_cost_usd,requests)
VALUES(?,?,?,?,?,?,?,?,?,?)`, cycleID, at, account, used, resetAt, 15, "pro", tokens, float64(tokens)/100, 1); err != nil {
		t.Fatal(err)
	}
}
