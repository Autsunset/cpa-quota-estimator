package main

import (
	"context"
	"path/filepath"
	"testing"
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
