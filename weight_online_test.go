package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestOnlineCycleScaleUpdatesAfterCrossing(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "online.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	fit := weightFit{Available: true, Lag: 0, RandomWalkSigma: .35,
		Models: []learnedModelWeights{{Model: "gpt-5.6-sol", Input: weightEstimate{Value: 1}, Cache: weightEstimate{Value: .1}, Output: weightEstimate{Value: 5}}},
		Fast:   weightEstimate{Value: 2.5}, LongContext: weightEstimate{Value: 1},
		CycleScales: []learnedCycleScale{{Account: "a", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 1000,
			Scale: weightEstimate{Value: .5, Low: .25, High: 1}, CreditsPerPercent: weightEstimate{Value: 200}}},
	}
	if err = s.seedOnlineCycleScales(context.Background(), &fit); err != nil {
		t.Fatal(err)
	}
	for index, percent := range []float64{0, 1} {
		value := percent
		e := event{RequestedAt: int64(100 + index*10), ObservedAt: int64(100 + index*10), Account: "a", Model: "gpt-5.6-sol",
			InputTokens: 1_000_000, TotalTokens: 1_000_000, UsedPercent: &value, ResetAt: 1000, WindowMinutes: 15,
			QuotaScope: mainQuotaScope, LearnedFit: &fit}
		if err = s.insertEvent(context.Background(), e, 5*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	var logScale float64
	var endpoint int64
	if err = s.db.QueryRow(`SELECT log_scale,last_end_event_id FROM online_cycle_scales WHERE account='a' AND window='main' AND cycle_id=1`).Scan(&logScale, &endpoint); err != nil {
		t.Fatal(err)
	}
	if endpoint == 0 || math.Exp(logScale) <= .5 {
		t.Fatalf("online scale=%g endpoint=%d", math.Exp(logScale), endpoint)
	}
}
