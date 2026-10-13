package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func historicalCycleFixture(t *testing.T) *store {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "history-boundary.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	// Older ingestion advanced the old cycle's schedule while keeping the
	// first unconfirmed readings of its replacement in the old raw ledger.
	if _, err = s.db.Exec(`INSERT INTO quota_cycles
		(id,account,started_at,ended_at,reset_at,window_minutes,plan_type,close_reason,
		first_sample_at,last_sample_at,start_used_percent,end_used_percent,peak_used_percent)
		VALUES(1,'history',100,300,5500,100,'pro','early_reset',100,200,80,85,85),
		(2,'history',300,0,5500,100,'pro','',300,400,0,1,1);
		INSERT INTO usage_events
		(id,cycle_id,account,requested_at,observed_at,used_percent,reset_at,window_minutes,total_tokens,cost_usd)
		VALUES(1,1,'history',100,100,80,5000,100,100,1),
		(2,1,'history',200,200,85,5000,100,100,1),
		(3,1,'history',250,250,0,5500,100,100,1),
		(4,1,'history',260,260,0,5500,100,100,1),
		(5,2,'history',300,300,0,5500,100,100,1),
		(6,2,'history',400,400,1,5500,100,100,1);
		INSERT INTO quota_samples
		(cycle_id,account,sampled_at,used_percent,reset_at,window_minutes,window_tokens,window_cost_usd,requests)
		VALUES(1,'history',100,80,5000,100,100,1,1),
		(1,'history',200,85,5000,100,200,2,2),
		(2,'history',300,0,5500,100,100,1,1),
		(2,'history',400,1,5500,100,200,2,2);`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHistoricalCycleDoesNotAppendUnconfirmedReplacementQuota(t *testing.T) {
	s := historicalCycleFixture(t)
	ctx := context.Background()
	before := quotaRawEvidenceFingerprint(t, s.db)
	cycles, err := s.cycles(ctx, "history", 10)
	if err != nil || len(cycles) != 2 || cycles[1].ResetAt != 5000 || cycles[0].ResetAt != 5500 {
		t.Fatalf("historical schedule follows unaccepted raw headers: cycles=%+v err=%v", cycles, err)
	}
	for _, limit := range []int{1, 100} {
		points, _, err := s.pointsForCycle(ctx, "history", 1, limit)
		if err != nil || len(points) == 0 {
			t.Fatalf("points=%+v err=%v", points, err)
		}
		last := points[len(points)-1]
		if last.UsedPercent != 85 || last.ResetAt != 5000 || last.Time != 200 {
			t.Fatalf("historical tail uses the replacement's fresh quota: %+v", last)
		}
	}
	a := &app{cfg: defaultConfig(), store: s}
	for _, endpoint := range []string{"summary", "series"} {
		response := a.handleManagement(managementRequest{
			Method: "GET", Path: "/cpa-quota-estimator/" + endpoint,
			Query: url.Values{"account": {"history"}, "cycle_id": {"1"}},
		})
		if response.StatusCode != 200 {
			t.Fatalf("%s status=%d body=%s", endpoint, response.StatusCode, response.Body)
		}
		var payload struct {
			Latest           quotaPoint   `json:"latest"`
			Points           []quotaPoint `json:"points"`
			RemainingPercent float64      `json:"remaining_percent"`
		}
		if err = json.Unmarshal(response.Body, &payload); err != nil {
			t.Fatal(err)
		}
		if endpoint == "summary" {
			if payload.Latest.UsedPercent != 85 || payload.RemainingPercent != 15 {
				t.Fatalf("historical summary reports a refill: %+v", payload)
			}
		} else if len(payload.Points) == 0 || payload.Points[len(payload.Points)-1].UsedPercent != 85 {
			t.Fatalf("historical series reports a refill: %+v", payload)
		}
	}
	if after := quotaRawEvidenceFingerprint(t, s.db); after != before {
		t.Fatal("reading historical forecasts changed the raw ledger")
	}
}

func TestHistoricalCycleAcceptsFreshTailOnlyInItsTrustedRegime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		at, seenAt int64
		used       float64
	}{
		{"valid_jittered_tail", 220, 225, 85},
		{"unaccepted_same_plan_drop", 220, 225, 0},
		{"at_closed_boundary", 300, 300, 85},
		{"after_closed_boundary", 350, 350, 85},
		{"observation_crosses_boundary", 290, 310, 85},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := historicalCycleFixture(t)
			if _, err := s.db.Exec(`INSERT INTO usage_events
				(cycle_id,account,requested_at,observed_at,used_percent,reset_at,window_minutes,total_tokens)
				VALUES(1,'history',?,?,?,5060,100,100)`, tc.at, tc.seenAt, tc.used); err != nil {
				t.Fatal(err)
			}
			points, _, err := s.pointsForCycle(context.Background(), "history", 1, 100)
			if err != nil {
				t.Fatal(err)
			}
			last := points[len(points)-1]
			wantTime := int64(200)
			if tc.name == "valid_jittered_tail" {
				wantTime = tc.seenAt
			}
			if last.Time != wantTime || last.UsedPercent != 85 {
				t.Fatalf("historical trusted tail=%+v want time=%d used=85", last, wantTime)
			}
		})
	}
}

func TestHistoricalCycleRejectsSampleAtClosedBoundary(t *testing.T) {
	s := historicalCycleFixture(t)
	if _, err := s.db.Exec(`INSERT INTO quota_samples
		(cycle_id,account,sampled_at,used_percent,reset_at,window_minutes)
		VALUES(1,'history',300,0,5500,100)`); err != nil {
		t.Fatal(err)
	}
	points, _, err := s.pointsForCycle(context.Background(), "history", 1, 100)
	if err != nil || len(points) == 0 {
		t.Fatalf("points=%+v err=%v", points, err)
	}
	last := points[len(points)-1]
	if last.UsedPercent != 85 || last.Time != 200 {
		t.Fatalf("historical tail includes a boundary sample from its replacement: %+v", last)
	}
}

func TestHistoricalCycleRangeDoesNotIncludeTouchingNextCycle(t *testing.T) {
	s := historicalCycleFixture(t)
	for _, tc := range []struct {
		start, end, cycle int64
	}{{100, 300, 1}, {300, 450, 2}} {
		points, cycles, err := s.pointsForRange(context.Background(), "history", tc.start, tc.end, 100)
		if err != nil || len(cycles) != 1 || cycles[0].ID != tc.cycle || len(points) == 0 {
			t.Fatalf("range [%d,%d) includes an adjacent cycle: cycles=%+v points=%+v err=%v",
				tc.start, tc.end, cycles, points, err)
		}
		for _, point := range points {
			if point.CycleID != tc.cycle || point.Time < tc.start || point.Time >= tc.end {
				t.Fatalf("point outside requested cycle/range: %+v", point)
			}
		}
	}
}

func TestHistoricalCycleRetainsConfirmedRecoveryWithoutSample(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "closed-recovery.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	for _, item := range []struct {
		at, reset int64
		used      float64
	}{{100, 10000, 49}, {200, 10000, 50}, {300, 20000, 70},
		{400, 20000, 71}, {500, 10000, 50}, {600, 10000, 51}} {
		if err = s.insertEvent(ctx, event{
			Account: "recovery", Model: "gpt-5.6-sol", RequestedAt: item.at, ObservedAt: item.at,
			ResetAt: item.reset, WindowMinutes: 100, UsedPercent: &item.used, TotalTokens: 100,
		}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`DELETE FROM quota_samples WHERE reset_at=10000 AND sampled_at>=500;
		UPDATE quota_cycles SET ended_at=700,close_reason='scheduled_reset'`); err != nil {
		t.Fatal(err)
	}
	cycles, err := s.cycles(ctx, "recovery", 10)
	if err != nil || len(cycles) != 1 || cycles[0].ResetAt != 10000 {
		t.Fatalf("an anomalous sample replaced the confirmed recovery schedule: cycles=%+v err=%v", cycles, err)
	}
	points, _, err := s.pointsForCycle(ctx, "recovery", cycles[0].ID, 100)
	if err != nil || len(points) == 0 {
		t.Fatalf("points=%+v err=%v", points, err)
	}
	last := points[len(points)-1]
	if last.ResetAt != 10000 || last.UsedPercent != 51 || last.Time != 600 || last.Anomalous {
		t.Fatalf("confirmed historical recovery was lost: %+v", last)
	}
}

func TestHistoricalCycleSnapshot(t *testing.T) {
	path := os.Getenv("HISTORICAL_CYCLE_SNAPSHOT")
	if path == "" {
		t.Skip("set HISTORICAL_CYCLE_SNAPSHOT to check historical tails in a backup read-only")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s := &store{db: db}
	ctx := context.Background()
	accounts, err := s.accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		cycles, err := s.cycles(ctx, account, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for index, cycle := range cycles {
			if cycle.Current {
				continue
			}
			var used float64
			var reset, window int64
			if err = db.QueryRow(`SELECT used_percent,reset_at,window_minutes
				FROM quota_samples WHERE cycle_id=? AND sampled_at<? ORDER BY sampled_at DESC,id DESC LIMIT 1`,
				cycle.ID, cycle.EndedAt).Scan(&used, &reset, &window); err != nil {
				t.Fatal(err)
			}
			points, _, err := s.pointsForCycle(ctx, account, cycle.ID, 10000)
			if err != nil || len(points) == 0 {
				t.Fatalf("cycle=%d points=%d err=%v", cycle.ID, len(points), err)
			}
			last := points[len(points)-1]
			if cycle.ResetAt != reset || cycle.WindowMinutes != window ||
				absInt64(last.ResetAt-reset) > segmentResetSlack || last.UsedPercent < used-resetPercentTolerance ||
				last.Time >= cycle.EndedAt {
				t.Fatalf("history=%d cycle=%d trusted_used=%g trusted_reset=%d schedule=%d last=%+v",
					index, cycle.ID, used, reset, cycle.ResetAt, last)
			}
			t.Logf("history=%d cycle=%d last_used=%g remaining=%g reset=%d",
				index, cycle.ID, last.UsedPercent, 100-last.UsedPercent, cycle.ResetAt)
		}
	}
}
