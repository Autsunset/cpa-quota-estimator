package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestScheduledFitReusesBacktestUntilDailyRefresh(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "scheduled-fit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = seedPrices(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	opts := defaultWeightLearnerOptions()
	previous := weightBacktest{
		GeneratedAt:   time.Now().Unix() - 100,
		SelectedLag:   1,
		Scores:        []backtestScore{{Mode: "sentinel", MAE: 123}},
		FittedWeights: weightFit{FittedAt: time.Now().Unix() - 100, RandomWalkSigma: opts.RandomWalkSigma, HalfLifeDays: opts.HalfLifeDays},
	}
	result, err := s.refreshWeightFitScheduled(context.Background(), opts, previous, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.GeneratedAt != previous.GeneratedAt || result.SelectedLag != 1 || len(result.Scores) != 1 || result.Scores[0].Mode != "sentinel" || result.FittedWeights.FittedAt < previous.FittedWeights.FittedAt {
		t.Fatalf("hourly fit unexpectedly reran backtest: %#v", result)
	}
	previous.GeneratedAt = time.Now().Unix() - 25*3600
	result, err = s.refreshWeightFitScheduled(context.Background(), opts, previous, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.GeneratedAt == previous.GeneratedAt || len(result.Lags) != 3 {
		t.Fatalf("daily backtest was skipped: %#v", result)
	}
}
