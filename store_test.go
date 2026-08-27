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
	if len(cycles) != 1 {
		t.Fatalf("first changed-schedule observation must not split the cycle: %#v", cycles)
	}
	insert(720, 1, 1300, 400)

	cycles, err = s.cycles(ctx, account, 10)
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
	if current.ActualTokens != 750 {
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
	insert(140, 1, 300)
	insert(170, 2, 400)

	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 {
		t.Fatalf("cycles = %#v", cycles)
	}
	current, previous := cycles[0], cycles[1]
	if current.StartedAt != 110 || current.ResetAt != 700 || current.ActualTokens != 900 {
		t.Fatalf("current cycle = %#v", current)
	}
	if previous.EndedAt != 110 || previous.CloseReason != "early_reset" || previous.ActualTokens != 100 {
		t.Fatalf("previous cycle = %#v", previous)
	}
	points, _, err := s.pointsForCycle(ctx, account, current.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].UsedPercent != 0 || points[1].UsedPercent != 2 || points[1].WindowTokens != 900 {
		t.Fatalf("new cycle points = %#v", points)
	}
}

func TestCycleRequiresThreeStableLowObservations(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "two-low-observations.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "two-low-observations-account"
	for _, item := range []struct {
		at   int64
		used float64
	}{{100, 40}, {110, 0}, {180, 1}} {
		e := event{RequestedAt: item.at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 100, UsedPercent: &item.used, ResetAt: 700, WindowMinutes: 10, PlanType: "pro"}
		if err = s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].ActualTokens != 300 {
		t.Fatalf("cycles = %#v", cycles)
	}
}

func TestCycleRejectsFailedLowObservation(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "failed-low-observation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "failed-low-observation-account"
	for _, item := range []struct {
		at     int64
		used   float64
		failed bool
	}{{100, 40, false}, {110, 0, false}, {140, 1, true}, {180, 2, false}} {
		e := event{RequestedAt: item.at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 100, Failed: item.failed, UsedPercent: &item.used, ResetAt: 700, WindowMinutes: 10, PlanType: "pro"}
		if err = s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].ActualTokens != 400 {
		t.Fatalf("cycles = %#v", cycles)
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

func TestSparkQuotaDoesNotAffectMainCycle(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "spark-scope.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "spark-scope-account"
	base := time.Date(2026, time.August, 18, 13, 0, 0, 0, shanghaiLocation()).Unix()
	mainReset := base + 7*24*60*60
	sparkReset := mainReset + 59*60

	main81 := 81.0
	if err = s.insertEvent(ctx, event{RequestedAt: base, Account: account, Provider: "openai", Model: "gpt-5.6-sol", TotalTokens: 100, CostUSD: 1, UsedPercent: &main81, ResetAt: mainReset, WindowMinutes: 10080, PlanType: "pro"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	spark0 := 0.0
	if err = s.insertEvent(ctx, event{RequestedAt: base + 10, Account: account, Provider: "openai", Model: "gpt-5.3-codex-spark", TotalTokens: 200, CostUSD: 2, UsedPercent: &spark0, ResetAt: sparkReset, WindowMinutes: 10080, PlanType: "pro", QuotaScope: quotaScopeForUsage("gpt-5.3-codex-spark", "")}, time.Minute); err != nil {
		t.Fatal(err)
	}
	main84 := 84.0
	if err = s.insertEvent(ctx, event{RequestedAt: base + 20, Account: account, Provider: "openai", Model: "gpt-5.6-sol", TotalTokens: 300, CostUSD: 3, UsedPercent: &main84, ResetAt: mainReset, WindowMinutes: 10080, PlanType: "pro"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].PeakPercent != 84 || cycles[0].ResetAt != mainReset || cycles[0].ActualTokens != 400 || cycles[0].ActualCostUSD != 4 || cycles[0].Requests != 2 {
		t.Fatalf("main cycles = %#v", cycles)
	}
	var sparkRows, sparkAssigned, samples int64
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN cycle_id<>0 THEN 1 ELSE 0 END),0) FROM usage_events WHERE account=? AND quota_scope=?`, account, sparkQuotaScope).Scan(&sparkRows, &sparkAssigned); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_samples WHERE account=?`, account).Scan(&samples); err != nil {
		t.Fatal(err)
	}
	if sparkRows != 1 || sparkAssigned != 0 || samples != 2 {
		t.Fatalf("spark rows=%d assigned=%d quota samples=%d", sparkRows, sparkAssigned, samples)
	}
	monthly, err := s.monthly(ctx, account, "2026-08")
	if err != nil {
		t.Fatal(err)
	}
	if monthly.ActualTokens != 400 || monthly.ActualCostUSD != 4 || monthly.Requests != 2 || len(monthly.Cycles) != 1 || monthly.Cycles[0].MonthTokens != 400 || monthly.Cycles[0].MonthCostUSD != 4 || monthly.Cycles[0].MonthRequests != 2 {
		t.Fatalf("monthly = %#v", monthly)
	}
	sparkMonthly, err := s.monthlyQuotaScopeAt(ctx, account, sparkQuotaScope, "2026-08", base+20)
	if err != nil {
		t.Fatal(err)
	}
	if sparkMonthly.ActualTokens != 200 || sparkMonthly.ActualCostUSD != 2 || sparkMonthly.Requests != 1 || sparkMonthly.CycleCount != 1 || sparkMonthly.Cycles[0].MonthTokens != 200 {
		t.Fatalf("Spark monthly = %#v", sparkMonthly)
	}
}

func TestQuotaScopeForUsageRecognizesSparkAlias(t *testing.T) {
	if got := quotaScopeForUsage("gpt-5.6-sol", "gpt-5.3-codex-spark"); got != sparkQuotaScope {
		t.Fatalf("Spark alias scope = %q", got)
	}
	if got := quotaScopeForUsage("gpt-5.6-sol", ""); got != mainQuotaScope {
		t.Fatalf("main scope = %q", got)
	}
}

func TestLatestSparkQuotaSeriesUsesItsOwnResetCycle(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "spark-series.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	account := "spark-series-account"
	for _, row := range []struct {
		at      int64
		used    float64
		resetAt int64
		failed  bool
	}{{110, 90, 400, false}, {210, 0, 900, false}, {220, 1, 900, true}, {240, 2, 900, false}} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,provider,model,used_percent,reset_at,window_minutes,plan_type,quota_scope,failed)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, row.at, row.at, account, "openai", "gpt-5.3-codex-spark", row.used, row.resetAt, 10, "pro", sparkQuotaScope, row.failed); err != nil {
			t.Fatal(err)
		}
	}
	mainUsed := 84.0
	if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,provider,model,used_percent,reset_at,window_minutes,plan_type,quota_scope)
VALUES(?,?,?,?,?,?,?,?,?,?)`, 230, 230, account, "openai", "gpt-5.6-sol", mainUsed, 800, 10, "pro", mainQuotaScope); err != nil {
		t.Fatal(err)
	}

	series, err := s.latestQuotaScopeSeriesAt(ctx, account, sparkQuotaScope, 100, 450)
	if err != nil {
		t.Fatal(err)
	}
	if series.Scope != sparkQuotaScope || series.ResetAt != 900 || series.StartedAt != 400 || series.WindowMinutes != 10 || len(series.Points) != 0 {
		// The two current-cycle samples above intentionally precede the declared
		// cycle start and therefore must not leak into the displayed cycle.
		t.Fatalf("series with out-of-cycle points = %#v", series)
	}

	for _, row := range []struct {
		at     int64
		used   float64
		tokens int64
		cost   float64
	}{{410, 2, 100, 1}, {450, 3, 300, 3}} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,provider,model,used_percent,reset_at,window_minutes,plan_type,quota_scope,total_tokens,cost_usd)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, row.at, row.at, account, "openai", "gpt-5.3-codex-spark", row.used, 900, 10, "pro", sparkQuotaScope, row.tokens, row.cost); err != nil {
			t.Fatal(err)
		}
	}
	series, err = s.latestQuotaScopeSeriesAt(ctx, account, sparkQuotaScope, 100, 450)
	if err != nil {
		t.Fatal(err)
	}
	if series.UsedPercent != 3 || series.ObservationCount != 2 || len(series.CapacityPoints) != 1 || len(series.Points) != 2 || series.Points[0].Time != 410 || series.Points[0].UsedPercent != 2 || series.Points[0].WindowTokens != 100 || series.Points[1].Time != 450 || series.Points[1].UsedPercent != 3 || series.Points[1].WindowTokens != 400 || series.Points[1].WindowCostUSD != 4 || series.Points[1].Requests != 2 {
		t.Fatalf("Spark series = %#v", series)
	}
	if !series.Estimate.Available || series.Estimate.FullWindowTokens != 30000 || series.Estimate.FullWindowCostUSD != 300 || series.Estimate.RemainingTokens != 29100 || series.Estimate.RemainingCostUSD != 291 {
		t.Fatalf("Spark estimate = %#v", series.Estimate)
	}
}

func TestExpiredSparkScheduleAdvancesWithoutInventingCurrentUsage(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "spark-expired-schedule.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	location := shanghaiLocation()
	stamp := func(day, hour, minute int) int64 {
		return time.Date(2026, time.August, day, hour, minute, 0, 0, location).Unix()
	}
	account := "spark-expired-schedule-account"
	oldReset := stamp(20, 18, 10)
	lastObserved := stamp(18, 20, 37)
	used := 8.0
	if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,model,total_tokens,cost_usd,failed,used_percent,reset_at,window_minutes,plan_type,quota_scope) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		lastObserved-1, lastObserved, account, "gpt-5.3-codex-spark", 100, 1, 0, used, oldReset, 10080, "pro", sparkQuotaScope); err != nil {
		t.Fatal(err)
	}

	now := stamp(21, 14, 0)
	series, err := s.latestQuotaScopeSeriesAt(ctx, account, sparkQuotaScope, 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if !series.ScheduleInferred || series.StartedAt != oldReset || series.ResetAt != stamp(27, 18, 10) || series.LastObservedAt != lastObserved {
		t.Fatalf("advanced Spark series = %#v", series)
	}
	if series.ObservationCount != 0 || series.UsedPercent != 0 || len(series.Points) != 0 || len(series.CapacityPoints) != 0 || series.Estimate.Available {
		t.Fatalf("inferred series invented current usage = %#v", series)
	}

	monthly, err := s.monthlyQuotaScopeAt(ctx, account, sparkQuotaScope, "2026-08", now)
	if err != nil {
		t.Fatal(err)
	}
	if monthly.CycleCount != 2 || monthly.ResetCount != 1 || monthly.AllocatedCycleCount != 2 || monthly.ActualTokens != 100 || monthly.Requests != 1 {
		t.Fatalf("advanced Spark monthly summary = %#v", monthly)
	}
	if len(monthly.Cycles) != 2 || !monthly.Cycles[0].Current || !monthly.Cycles[0].ScheduleInferred || monthly.Cycles[0].StartedAt != oldReset || monthly.Cycles[0].ResetAt != stamp(27, 18, 10) || monthly.Cycles[0].MonthRequests != 0 {
		t.Fatalf("inferred current Spark cycle = %#v", monthly.Cycles)
	}
	previous := monthly.Cycles[1]
	if previous.Current || previous.EndedAt != oldReset || previous.CloseReason != "scheduled_reset" || previous.PeakPercent != 8 || previous.MonthRequests != 1 {
		t.Fatalf("expired Spark cycle was not closed = %#v", previous)
	}

	firstCurrentObservation := stamp(21, 14, 5)
	currentUsed := 0.0
	if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,model,total_tokens,cost_usd,failed,used_percent,reset_at,window_minutes,plan_type,quota_scope) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		firstCurrentObservation-1, firstCurrentObservation, account, "gpt-5.3-codex-spark", 200, 2, 0, currentUsed, stamp(27, 18, 10), 10080, "pro", sparkQuotaScope); err != nil {
		t.Fatal(err)
	}
	series, err = s.latestQuotaScopeSeriesAt(ctx, account, sparkQuotaScope, 100, now)
	if err != nil {
		t.Fatal(err)
	}
	if !series.ScheduleInferred || series.ObservationCount != 1 || series.UsedPercent != 0 || len(series.Points) != 1 || series.LastObservedAt != firstCurrentObservation || series.Points[0].WindowTokens != 200 {
		t.Fatalf("single unconfirmed current observation = %#v", series)
	}
}

func TestSparkScheduleCorrectionDoesNotCreateOverlappingCycle(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "spark-schedule-correction.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	location := shanghaiLocation()
	stamp := func(day, hour, minute int) int64 {
		return time.Date(2026, time.August, day, hour, minute, 0, 0, location).Unix()
	}
	account := "spark-schedule-correction-account"
	oldReset := stamp(20, 11, 29)
	correctedReset := stamp(20, 18, 10)
	for _, row := range []struct {
		at      int64
		used    float64
		resetAt int64
		tokens  int64
		cost    float64
	}{
		{stamp(14, 0, 7), 3, oldReset, 100, 1},
		{stamp(14, 0, 32), 6, oldReset, 200, 2},
		{stamp(17, 10, 50), 6, correctedReset, 300, 3},
		{stamp(18, 16, 59), 7, correctedReset, 400, 4},
	} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,model,total_tokens,cost_usd,failed,used_percent,reset_at,window_minutes,plan_type,quota_scope) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			row.at, row.at, account, "gpt-5.3-codex-spark", row.tokens, row.cost, 0, row.used, row.resetAt, 10080, "pro", sparkQuotaScope); err != nil {
			t.Fatal(err)
		}
	}
	cycles, err := s.quotaScopeCycles(ctx, account, sparkQuotaScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].ResetAt != correctedReset || cycles[0].StartedAt != stamp(13, 18, 10) || cycles[0].PeakPercent != 7 || !cycles[0].Current {
		t.Fatalf("Spark corrected cycles = %#v", cycles)
	}
	series, err := s.latestQuotaScopeSeriesAt(ctx, account, sparkQuotaScope, 100, stamp(18, 17, 0))
	if err != nil {
		t.Fatal(err)
	}
	if series.ObservationCount != 4 || series.UsedPercent != 7 || len(series.Points) != 3 || series.Points[len(series.Points)-1].WindowTokens != 1000 || series.Points[len(series.Points)-1].Requests != 4 {
		t.Fatalf("Spark corrected series = %#v", series)
	}
	monthly, err := s.monthlyQuotaScopeAt(ctx, account, sparkQuotaScope, "2026-08", stamp(18, 17, 0))
	if err != nil {
		t.Fatal(err)
	}
	if monthly.CycleCount != 1 || monthly.ResetCount != 0 || monthly.ActualTokens != 1000 || monthly.Requests != 4 || monthly.Cycles[0].MonthTokens != 1000 || monthly.Cycles[0].MonthRequests != 4 || monthly.ConsumedQuotaPercent != 7 {
		t.Fatalf("Spark corrected monthly = %#v", monthly)
	}
}

func TestSparkMonthlySummaryUsesIndependentCyclesAndCapacity(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "spark-monthly.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	location := shanghaiLocation()
	stamp := func(day int) int64 { return time.Date(2026, time.January, day, 12, 0, 0, 0, location).Unix() }
	account := "spark-monthly-account"
	if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,model,total_tokens,cost_usd,quota_scope) VALUES(?,?,?,?,?,?,?)`, stamp(5), stamp(5), account, "gpt-5.6-sol", 999, 99, mainQuotaScope); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		at      int64
		used    float64
		resetAt int64
		tokens  int64
		cost    float64
	}{
		{stamp(1), 10, stamp(8), 100, 1},
		{stamp(7), 50, stamp(8), 200, 2},
		{stamp(8), 5, stamp(15), 300, 3},
		{stamp(14), 25, stamp(15), 400, 4},
	} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,model,total_tokens,cost_usd,failed,used_percent,reset_at,window_minutes,plan_type,quota_scope) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			row.at, row.at, account, "gpt-5.3-codex-spark", row.tokens, row.cost, 0, row.used, row.resetAt, 10080, "pro", sparkQuotaScope); err != nil {
			t.Fatal(err)
		}
	}
	mainMonthly, err := s.monthly(ctx, account, "2026-01")
	if err != nil {
		t.Fatal(err)
	}
	if mainMonthly.ActualTokens != 999 || mainMonthly.ActualCostUSD != 99 || mainMonthly.Requests != 1 {
		t.Fatalf("main monthly includes Spark = %#v", mainMonthly)
	}
	summary, err := s.monthlyQuotaScopeAt(ctx, account, sparkQuotaScope, "2026-01", stamp(14)+60)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ActualTokens != 1000 || summary.ActualCostUSD != 10 || summary.Requests != 4 {
		t.Fatalf("Spark actual usage = %#v", summary)
	}
	if summary.CycleCount != 2 || summary.ResetCount != 1 || summary.EarlyResetCount != 0 || summary.ConsumedQuotaPercent != 75 || summary.ConsumedQuotaEquivalent != .75 {
		t.Fatalf("Spark cycles = %#v", summary)
	}
	if summary.AllocatedCycleCount != 2 || summary.EstimatedCycleCount != 2 || summary.EstimatedTokens != 2500 || summary.EstimatedCostUSD != 25 || len(summary.Cycles) != 2 {
		t.Fatalf("Spark capacity = %#v", summary)
	}
	if summary.Cycles[0].MonthTokens != 700 || summary.Cycles[1].MonthTokens != 300 {
		t.Fatalf("Spark cycle usage = %#v", summary.Cycles)
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
	}{{stamp(time.January, 31, 12, 0), 40, 100}, {stamp(time.February, 1, 1, 0), 0, 200}, {stamp(time.February, 1, 1, 1), 1, 300}, {stamp(time.February, 1, 1, 2), 2, 400}} {
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
	if summary.ActualTokens != 900 || summary.Requests != 3 || summary.ConsumedQuotaPercent != 2 || !summary.QuotaCoverageComplete {
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

func TestExhaustedFiveHourCycleStartsFreshWindowWithoutCarryingPeak(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "five-hour-exhausted.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	const (
		account       = "five-hour-exhausted-account"
		windowMinutes = int64(300)
		oldResetAt    = int64(19_000)
	)
	insert := func(at int64, used float64, resetAt int64, tokens int64) {
		t.Helper()
		e := event{RequestedAt: at, Account: account, Provider: "openai", Model: "gpt", TotalTokens: tokens, UsedPercent: &used, ResetAt: resetAt, WindowMinutes: windowMinutes, PlanType: "plus"}
		if err := s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	insert(18_000, 99, oldResetAt, 100)
	insert(18_900, 100, oldResetAt, 200)

	// An unstarted replacement window initially carries the exhausted 100%
	// value and reports a reset exactly five hours after this observation.
	insert(19_010, 100, 37_010, 300)
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 {
		t.Fatalf("cycles after first advanced reset = %#v", cycles)
	}
	current, previous := cycles[0], cycles[1]
	if !current.Current || current.StartedAt != 19_010 || current.ResetAt != 37_010 || current.FirstSampleAt != 0 || current.PeakPercent != 0 {
		t.Fatalf("pending current cycle = %#v", current)
	}
	if previous.Current || previous.EndedAt != oldResetAt || previous.CloseReason != "scheduled_reset" || previous.PeakPercent != 100 {
		t.Fatalf("previous exhausted cycle = %#v", previous)
	}

	// Until the new window is really activated, reset_at can keep sliding.
	// The carried 100% must remain untrusted and must not become a sample.
	insert(19_380, 100, 37_380, 400)
	insert(19_390, 12, 37_380, 500)

	cycles, err = s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 {
		t.Fatalf("cycles after fresh sample = %#v", cycles)
	}
	current = cycles[0]
	if current.StartedAt != 19_380 || current.ResetAt != 37_380 || current.StartPercent != 12 || current.EndPercent != 12 || current.PeakPercent != 12 {
		t.Fatalf("fresh current cycle = %#v", current)
	}
	points, _, err := s.pointsForCycle(ctx, account, current.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].UsedPercent != 12 {
		t.Fatalf("fresh cycle points = %#v", points)
	}
}

func TestExistingExhaustedScheduleCarryoverRepairsOnFreshReading(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "five-hour-carryover-repair.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	const (
		account       = "five-hour-carryover-repair-account"
		windowMinutes = int64(300)
		oldResetAt    = int64(19_000)
		newResetAt    = int64(37_380)
	)
	result, err := s.db.ExecContext(ctx, `INSERT INTO quota_cycles(account,started_at,reset_at,window_minutes,plan_type,first_sample_at,last_sample_at,start_used_percent,end_used_percent,peak_used_percent) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		account, 1_000, newResetAt, windowMinutes, "plus", 18_000, 19_380, 99, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	cycleID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		at      int64
		used    float64
		resetAt int64
		tokens  int64
	}{{18_000, 99, oldResetAt, 100}, {18_900, 100, oldResetAt, 200}, {19_010, 100, 37_010, 300}, {19_380, 100, newResetAt, 400}} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(cycle_id,requested_at,observed_at,account,provider,model,total_tokens,used_percent,reset_at,window_minutes,plan_type,quota_scope) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			cycleID, row.at, row.at, account, "openai", "gpt", row.tokens, row.used, row.resetAt, windowMinutes, "plus", mainQuotaScope); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		at      int64
		used    float64
		resetAt int64
		tokens  int64
	}{{18_000, 99, oldResetAt, 100}, {18_900, 100, oldResetAt, 300}, {19_380, 100, newResetAt, 1_000}} {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes,plan_type,window_tokens,requests) VALUES(?,?,?,?,?,?,?,?,?)`,
			cycleID, row.at, account, row.used, row.resetAt, windowMinutes, "plus", row.tokens, 1); err != nil {
			t.Fatal(err)
		}
	}

	freshUsed := 12.0
	if err = s.insertEvent(ctx, event{RequestedAt: 19_390, Account: account, Provider: "openai", Model: "gpt", TotalTokens: 500, UsedPercent: &freshUsed, ResetAt: newResetAt, WindowMinutes: windowMinutes, PlanType: "plus"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 2 {
		t.Fatalf("repaired cycles = %#v", cycles)
	}
	current, previous := cycles[0], cycles[1]
	if !current.Current || current.StartedAt != 19_380 || current.ResetAt != newResetAt || current.StartPercent != 12 || current.PeakPercent != 12 {
		t.Fatalf("repaired current cycle = %#v", current)
	}
	if previous.Current || previous.EndedAt != oldResetAt || previous.CloseReason != "scheduled_reset" || previous.LastSampleAt != 18_900 || previous.PeakPercent != 100 {
		t.Fatalf("repaired previous cycle = %#v", previous)
	}
	var staleSamples int
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM quota_samples WHERE cycle_id=? AND sampled_at>=?`, previous.ID, oldResetAt).Scan(&staleSamples); err != nil {
		t.Fatal(err)
	}
	if staleSamples != 0 {
		t.Fatalf("previous cycle kept %d post-reset samples", staleSamples)
	}
}

func TestFiveHourAndWeeklyQuotaAreCalculatedIndependently(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "five-hour-weekly.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	location := shanghaiLocation()
	base := time.Date(2026, time.August, 1, 0, 0, 0, 0, location).Unix()
	primaryReset := base + 300*60
	weeklyReset := base + 10080*60
	account := "five-hour-weekly-account"

	for index, row := range []struct {
		primary   float64
		secondary float64
		tokens    int64
		cost      float64
	}{{10, 2, 100, 1}, {20, 3, 200, 2}, {30, 4, 300, 3}} {
		at := base + int64(index+1)*100
		primary, secondary := row.primary, row.secondary
		e := event{
			RequestedAt: at, Account: account, Provider: "openai", Model: "gpt",
			TotalTokens: row.tokens, CostUSD: row.cost,
			UsedPercent: &primary, ResetAt: primaryReset, WindowMinutes: 300,
			SecondaryUsedPercent: &secondary, SecondaryResetAt: weeklyReset, SecondaryWindowMinutes: 10080,
			PlanType: "plus",
		}
		if err = s.insertEvent(ctx, e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	// A historical Pro-style weekly-primary row in the same month must not be
	// counted in the Plus secondary weekly scope.
	if _, err = s.db.ExecContext(ctx, `INSERT INTO usage_events(requested_at,observed_at,account,provider,model,total_tokens,cost_usd,used_percent,reset_at,window_minutes,plan_type,quota_scope) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		base+50, base+50, account, "openai", "gpt", 999, 99, 50, weeklyReset, 10080, "pro", mainQuotaScope); err != nil {
		t.Fatal(err)
	}

	detected, err := s.hasFiveHourWeeklyQuota(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	if !detected {
		t.Fatal("five-hour primary plus weekly secondary quota was not detected")
	}
	cycles, err := s.cycles(ctx, account, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycles) != 1 || cycles[0].WindowMinutes != 300 || cycles[0].ResetAt != primaryReset || cycles[0].PeakPercent != 30 {
		t.Fatalf("primary five-hour cycles = %#v", cycles)
	}
	weekly, err := s.latestQuotaScopeSeriesAt(ctx, account, weeklyQuotaScope, 100, base+400)
	if err != nil {
		t.Fatal(err)
	}
	if weekly.Scope != weeklyQuotaScope || weekly.WindowMinutes != 10080 || weekly.ResetAt != weeklyReset || weekly.UsedPercent != 4 || weekly.ObservationCount != 3 {
		t.Fatalf("weekly quota series = %#v", weekly)
	}
	if len(weekly.Points) != 3 {
		t.Fatalf("weekly points = %#v", weekly.Points)
	}
	last := weekly.Points[len(weekly.Points)-1]
	if last.WindowTokens != 600 || last.WindowCostUSD != 6 || last.Requests != 3 {
		t.Fatalf("weekly cumulative usage = %#v", last)
	}
	monthly, err := s.monthlyQuotaScopeAt(ctx, account, weeklyQuotaScope, "2026-08", base+400)
	if err != nil {
		t.Fatal(err)
	}
	if monthly.ActualTokens != 600 || monthly.ActualCostUSD != 6 || monthly.Requests != 3 || monthly.CycleCount != 1 || monthly.ConsumedQuotaPercent != 4 {
		t.Fatalf("weekly monthly summary = %#v", monthly)
	}
}

func TestWeeklyQuotaStaysDisabledWithoutFiveHourPrimary(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "no-five-hour-weekly.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	used, secondary := 10.0, 20.0
	e := event{
		RequestedAt: 1_000, Account: "weekly-only-account", Provider: "openai", Model: "gpt", TotalTokens: 100,
		UsedPercent: &used, ResetAt: 605_800, WindowMinutes: 10080,
		SecondaryUsedPercent: &secondary, SecondaryResetAt: 1_210_600, SecondaryWindowMinutes: 10080,
		PlanType: "plus",
	}
	if err = s.insertEvent(ctx, e, time.Minute); err != nil {
		t.Fatal(err)
	}
	detected, err := s.hasFiveHourWeeklyQuota(ctx, e.Account)
	if err != nil {
		t.Fatal(err)
	}
	if detected {
		t.Fatal("weekly scope must stay disabled when the primary window is not five hours")
	}
}
