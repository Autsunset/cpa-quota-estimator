package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestScheduledResetAfterIdleGap(t *testing.T) {
	for _, gap := range []int64{15 * 60, 86400, 15 * 86400} {
		for _, oldUsed := range []float64{0, 2, 98} {
			t.Run(fmt.Sprintf("gap_%d_peak_%g", gap, oldUsed), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "idle-reset.sqlite")
				s, err := openStore(path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { s.close() }()
				ctx := context.Background()
				const account = "idle-weekly-account"
				const oldReset = int64(1_000_000)
				const window = int64(10080)
				newStart := oldReset + gap
				newReset := newStart + window*60
				insert := func(at int64, used float64, reset int64) {
					t.Helper()
					if err := s.insertEvent(ctx, event{Account: account, RequestedAt: at, ObservedAt: at + 5, UsedPercent: &used,
						ResetAt: reset, WindowMinutes: window, PlanType: "pro", TotalTokens: 100, CostUSD: 1}, time.Minute); err != nil {
						t.Fatal(err)
					}
				}
				insert(oldReset-100, oldUsed, oldReset)
				// Usage without quota headers after expiry must follow the new
				// ledger, including requests in the gap before its activation.
				if err := s.insertEvent(ctx, event{Account: account, RequestedAt: oldReset + 10, TotalTokens: 50, CostUSD: .5}, time.Minute); err != nil {
					t.Fatal(err)
				}
				insert(newStart+13, 0, newReset)
				cycles, err := s.cycles(ctx, account, 10)
				if err != nil || len(cycles) != 1 || cycles[0].ResetAt != oldReset || cycles[0].PeakPercent != oldUsed {
					t.Fatalf("first observation changed the old schedule or peak: %+v, %v", cycles, err)
				}
				// The pending confirmation must survive a plugin restart.
				if err := s.close(); err != nil {
					t.Fatal(err)
				}
				s, err = openStore(path)
				if err != nil {
					t.Fatal(err)
				}
				insert(newStart+30, 1, newReset)
				cycles, err = s.cycles(ctx, account, 10)
				if err != nil || len(cycles) != 2 {
					t.Fatalf("confirmed idle reset did not split: %+v, %v", cycles, err)
				}
				current, previous := cycles[0], cycles[1]
				if previous.EndedAt != oldReset || previous.ResetAt != oldReset || previous.CloseReason != "scheduled_reset" ||
					previous.PeakPercent != oldUsed || previous.ActualTokens != 100 || previous.ActualCostUSD != 1 || previous.Requests != 1 {
					t.Fatalf("old cycle = %+v", previous)
				}
				if current.StartedAt != newStart || current.ResetAt != newReset || !current.Current || current.PeakPercent != 1 ||
					current.ActualTokens != 250 || current.ActualCostUSD != 2.5 || current.Requests != 3 {
					t.Fatalf("new cycle = %+v", current)
				}
				points, _, err := s.pointsForCycle(ctx, account, current.ID, 10)
				if err != nil || len(points) == 0 || points[0].WindowTokens != 250 || points[0].WindowCostUSD != 2.5 {
					t.Fatalf("new ledger samples include old usage: %+v, %v", points, err)
				}
			})
		}
	}
}

func TestDelayedScheduledResetRequiresConsistentSuccessfulEvidence(t *testing.T) {
	for _, interruption := range []string{"failed", "different_schedule", "spark", "out_of_order"} {
		t.Run(interruption, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "confirmation.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			ctx := context.Background()
			const oldReset = int64(1_000_000)
			const newStart = oldReset + 900
			const newReset = newStart + 10080*60
			used := 98.0
			e := event{Account: "confirmation", RequestedAt: oldReset - 100, UsedPercent: &used, ResetAt: oldReset, WindowMinutes: 10080, PlanType: "pro"}
			insert := func(e event) {
				t.Helper()
				if err := s.insertEvent(ctx, e, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			insert(e)
			used = 0
			e.RequestedAt, e.ResetAt = newStart+13, newReset
			insert(e)
			e.RequestedAt += 10
			switch interruption {
			case "failed":
				e.Failed = true
			case "different_schedule":
				e.ResetAt += 60
			case "spark":
				e.Model = "gpt-5.3-codex-spark"
			case "out_of_order":
				e.ObservedAt = newStart + 12
			}
			insert(e)
			cycles, err := s.cycles(ctx, e.Account, 10)
			if err != nil || len(cycles) != 1 || cycles[0].ResetAt != oldReset || cycles[0].PeakPercent != 98 {
				t.Fatalf("unconfirmed observation changed cycle: %+v, %v", cycles, err)
			}
		})
	}
}

func TestDelayedScheduledResetRejectsInvalidWindow(t *testing.T) {
	const oldReset = int64(1_000_000)
	current := quotaCycle{ResetAt: oldReset, WindowMinutes: 10080, PlanType: "pro"}
	used := 0.0
	valid := event{RequestedAt: oldReset + 900, UsedPercent: &used, ResetAt: oldReset + 900 + 10080*60, WindowMinutes: 10080, PlanType: "pro"}
	if !scheduledResetObservation(current, valid) {
		t.Fatal("idle replacement window should be a candidate")
	}
	for _, kind := range []string{"before_expiry", "future_start", "overlapping_window", "changed_window", "changed_plan", "failed"} {
		t.Run(kind, func(t *testing.T) {
			e := valid
			switch kind {
			case "before_expiry":
				e.RequestedAt = oldReset - 1
			case "future_start":
				e.ResetAt += 301
			case "overlapping_window":
				e.ResetAt = oldReset - 301 + 10080*60
			case "changed_window":
				e.WindowMinutes = 300
				e.ResetAt = e.RequestedAt + 300*60
			case "changed_plan":
				e.PlanType = "plus"
			case "failed":
				e.Failed = true
			}
			if scheduledResetObservation(current, e) {
				t.Fatal("invalid delayed replacement accepted")
			}
		})
	}
}
