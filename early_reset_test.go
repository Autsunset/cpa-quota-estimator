package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestWeeklyEarlyResetWithDenseObservations(t *testing.T) {
	for _, changeSchedule := range []bool{false, true} {
		name := "same_schedule"
		if changeSchedule {
			name = "new_schedule"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "early-reset.sqlite")
			s, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.close() }()
			ctx := context.Background()
			const account = "weekly-early-reset"
			const oldReset = int64(1789435500)
			const firstLow = int64(1789200720)
			newReset := oldReset
			if changeSchedule {
				newReset = firstLow + 7*24*60*60
			}
			insert := func(at int64, used float64, reset int64) {
				t.Helper()
				if err := s.insertEvent(ctx, event{
					Account: account, RequestedAt: at, ObservedAt: at,
					UsedPercent: &used, ResetAt: reset, WindowMinutes: 10080, PlanType: "pro",
					TotalTokens: 100, CostUSD: 1,
				}, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			insert(firstLow-3600, 91, oldReset)
			// Every adjacent group of three observations spans only 20 seconds.
			// Confirmation must retain the beginning of the whole low run.
			for offset := int64(0); offset <= 60; offset += 10 {
				insert(firstLow+offset, 0, newReset)
				cycles, err := s.cycles(ctx, account, 10)
				if err != nil {
					t.Fatal(err)
				}
				if offset < 60 {
					if len(cycles) != 1 || cycles[0].ResetAt != oldReset || cycles[0].PeakPercent != 91 {
						t.Fatalf("unconfirmed reset discarded the old cycle baseline: %#v", cycles)
					}
					if offset == 30 {
						if err = s.close(); err != nil {
							t.Fatal(err)
						}
						// Confirmation evidence must survive a plugin restart.
						s, err = openStore(path)
						if err != nil {
							t.Fatal(err)
						}
					}
					continue
				}
				if len(cycles) != 2 {
					t.Fatalf("confirmed early reset did not split the cycle: %#v", cycles)
				}
				current, previous := cycles[0], cycles[1]
				if current.StartedAt != firstLow || current.ResetAt != newReset || current.ActualTokens != 700 || current.Requests != 7 || current.ActualCostUSD != 7 {
					t.Fatalf("new cycle = %#v", current)
				}
				if previous.EndedAt != firstLow || previous.CloseReason != "early_reset" || previous.ResetAt != oldReset || previous.PeakPercent != 91 || previous.ActualTokens != 100 {
					t.Fatalf("previous cycle = %#v", previous)
				}
				confirmed, _, err := s.historicalEarlyResetConfirmed(ctx, previous, current)
				if err != nil || !confirmed {
					t.Fatalf("historical repair rejected a confirmed dense reset: confirmed=%v err=%v", confirmed, err)
				}
				points, _, err := s.pointsForCycle(ctx, account, current.ID, 100)
				if err != nil {
					t.Fatal(err)
				}
				if len(points) != 2 || points[0].Time != firstLow || points[0].WindowTokens != 100 || points[1].WindowTokens != 700 {
					t.Fatalf("new cycle samples = %#v", points)
				}
			}
			month, err := s.monthly(ctx, account, "2026-09")
			if err != nil {
				t.Fatal(err)
			}
			if month.EarlyResetCount != 1 || month.ActualTokens != 800 || month.Requests != 8 {
				t.Fatalf("monthly totals = %#v", month)
			}
		})
	}
}

func TestWeeklyEarlyResetRejectsReboundAndRegimeRecovery(t *testing.T) {
	for _, restoreEarlierSchedule := range []bool{false, true} {
		name := "low_reading_rebounds"
		if restoreEarlierSchedule {
			name = "earlier_schedule_recovers"
		}
		t.Run(name, func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "rebound.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			ctx := context.Background()
			const account = "weekly-rebound"
			const at = int64(1789200720)
			const originalReset = int64(1789435500)
			const alternateReset = at + 7*24*60*60
			insert := func(offset int64, used float64, reset int64) {
				t.Helper()
				if err := s.insertEvent(ctx, event{Account: account, RequestedAt: at + offset,
					UsedPercent: &used, ResetAt: reset, WindowMinutes: 10080, PlanType: "pro", TotalTokens: 100,
				}, time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			if restoreEarlierSchedule {
				insert(-120, 0, originalReset)
				insert(-90, 1, originalReset)
				insert(-60, 90, alternateReset)
				insert(-30, 91, alternateReset)
				insert(0, 1, originalReset)
				insert(30, 1, originalReset)
				insert(60, 2, originalReset)
			} else {
				insert(-60, 91, originalReset)
				insert(0, 0, alternateReset)
				insert(10, 0, alternateReset)
				insert(30, 91, originalReset)
				insert(60, 92, originalReset)
			}
			cycles, err := s.cycles(ctx, account, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(cycles) != 1 || cycles[0].ResetAt != originalReset {
				t.Fatalf("temporary quota change created a cycle: %#v", cycles)
			}
		})
	}
}
