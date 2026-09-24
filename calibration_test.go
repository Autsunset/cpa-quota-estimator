package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func calibrationTestStore(t *testing.T) (*store, func(int64, float64, string, float64)) {
	t.Helper()
	s, err := openStore(filepath.Join(t.TempDir(), "calibration.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	const first = int64(1_000_000)
	const reset = first + 7*86400
	insert := func(at int64, used float64, model string, value float64) {
		t.Helper()
		if err := s.insertEvent(context.Background(), event{Account: "synthetic", RequestedAt: at, ObservedAt: at, Model: model, UsedPercent: &used, ResetAt: reset, WindowMinutes: 10080, PlanType: "pro", CostUSD: value, TotalTokens: 100}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	insert(first, 0, "gpt-5.6-sol", 0)
	return s, insert
}

func TestGuidedCalibrationAdvancesAndMeasuresPollution(t *testing.T) {
	s, insert := calibrationTestStore(t)
	defer s.close()
	ctx := context.Background()
	session, err := s.startCalibration(ctx, "synthetic", "gpt-5.6-sol", "gpt-6-astra", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	const first = int64(1_000_000)
	insert(first+10, 1, "gpt-5.6-sol", 90)
	insert(first+20, 1, "gpt-6-astra", 12)
	insert(first+80, 2, "gpt-5.6-sol", 100)
	session, _, err = s.calibrationSession(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != "phase2" || session.PhaseA.Crossings != 2 || session.PhaseA.ConsumedPercent != 2 || session.PhaseB.StartPercent != 2 {
		t.Fatalf("phase transition=%#v", session)
	}
	want := 100 * 12.0 / 202.0
	if math.Abs(session.PhaseA.ContaminationPercent-want) > 1e-8 || !session.PhaseA.PollutionWarning {
		t.Fatalf("pollution=%#v", session.PhaseA)
	}
	insert(first+90, 3, "gpt-6-astra", 50)
	insert(first+150, 4, "gpt-6-astra", 50)
	session, _, err = s.calibrationSession(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != "completed" || session.RefitStatus != "pending" || session.PhaseB.Crossings != 2 || session.PhaseB.ContaminationPercent != 0 || session.PhaseA.EndEventID >= session.PhaseB.StartEventID {
		t.Fatalf("completed=%#v", session)
	}
}

func TestGuidedCalibrationInvalidatesAcrossReset(t *testing.T) {
	s, insert := calibrationTestStore(t)
	defer s.close()
	ctx := context.Background()
	session, err := s.startCalibration(ctx, "synthetic", "gpt-5.6-sol", "gpt-6-astra", 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	insert(1_000_010, 1, "gpt-5.6-sol", 10)
	if _, err = s.db.Exec(`UPDATE quota_cycles SET ended_at=?,close_reason='scheduled_reset' WHERE id=?`, 1_000_020, session.CycleID); err != nil {
		t.Fatal(err)
	}
	used := float64(0)
	if err = s.insertEvent(ctx, event{Account: "synthetic", RequestedAt: 1_000_020, ObservedAt: 1_000_020, Model: "gpt-5.6-sol", UsedPercent: &used, ResetAt: 1_000_020 + 7*86400, WindowMinutes: 10080, PlanType: "pro", CostUSD: 1, TotalTokens: 100}, time.Minute); err != nil {
		t.Fatal(err)
	}
	session, _, err = s.calibrationSession(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != "invalid" || session.InvalidReason != "quota_reset" || session.RefitStatus != "" {
		t.Fatalf("cross-reset session=%#v", session)
	}
}
