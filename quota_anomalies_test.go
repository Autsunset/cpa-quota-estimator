package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuotaRegimeAnomalyResetTolerance(t *testing.T) {
	for _, tc := range []struct {
		name          string
		resetDelta    int64
		windowDelta   int64
		wantAnomalies int
	}{
		{"minute_forward", 60, 0, 0},
		{"minute_backward", -60, 0, 0},
		{"slack_forward", segmentResetSlack, 0, 0},
		{"slack_backward", -segmentResetSlack, 0, 0},
		{"beyond_slack_forward", segmentResetSlack + 1, 0, 1},
		{"beyond_slack_backward", -segmentResetSlack - 1, 0, 1},
		{"window_change", 0, 1, 1},
		{"large_reset_change", 252120, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const reset = int64(10000)
			observations := []quotaRegimeObservation{
				{RequestedAt: 100, UsedPercent: 10, ResetAt: reset, WindowMinutes: 10080},
				{RequestedAt: 200, UsedPercent: 11, ResetAt: reset, WindowMinutes: 10080},
				{RequestedAt: 300, UsedPercent: 12, ResetAt: reset + tc.resetDelta, WindowMinutes: 10080 + tc.windowDelta},
				{RequestedAt: 400, UsedPercent: 13, ResetAt: reset + tc.resetDelta, WindowMinutes: 10080 + tc.windowDelta},
				{RequestedAt: 500, UsedPercent: 14, ResetAt: reset, WindowMinutes: 10080},
				{RequestedAt: 600, UsedPercent: 15, ResetAt: reset, WindowMinutes: 10080},
			}
			if anomalies := detectQuotaRegimeAnomalies(1, observations); len(anomalies) != tc.wantAnomalies {
				t.Fatalf("reset delta %d, window delta %d: anomalies=%+v, want %d",
					tc.resetDelta, tc.windowDelta, anomalies, tc.wantAnomalies)
			}
		})
	}
}

func TestQuotaRegimeAnomalyToleranceDoesNotChainResetDrift(t *testing.T) {
	var observations []quotaRegimeObservation
	for _, delta := range []int64{0, 60, 120, 180, 0} {
		for i := 0; i < 2; i++ {
			at := int64(len(observations)+1) * 100
			observations = append(observations, quotaRegimeObservation{
				RequestedAt: at, UsedPercent: float64(len(observations)),
				ResetAt: 10000 + delta, WindowMinutes: 10080,
			})
		}
	}
	anomalies := detectQuotaRegimeAnomalies(1, observations)
	if len(anomalies) != 1 || anomalies[0].StartedAt != 700 || anomalies[0].ObservationCount != 2 {
		t.Fatalf("gradual drift hid a change beyond the original reset tolerance: %+v", anomalies)
	}
}

func TestQuotaRegimeAnomalyRecoveryAllowsResetJitter(t *testing.T) {
	observations := []quotaRegimeObservation{
		{RequestedAt: 100, UsedPercent: 49, ResetAt: 10000, WindowMinutes: 10080},
		{RequestedAt: 200, UsedPercent: 50, ResetAt: 10060, WindowMinutes: 10080},
		{RequestedAt: 300, UsedPercent: 70, ResetAt: 20000, WindowMinutes: 10080},
		{RequestedAt: 400, UsedPercent: 71, ResetAt: 20060, WindowMinutes: 10080},
		{RequestedAt: 500, UsedPercent: 50, ResetAt: 10060, WindowMinutes: 10080},
		{RequestedAt: 600, UsedPercent: 51, ResetAt: 10000, WindowMinutes: 10080},
	}
	anomalies := detectQuotaRegimeAnomalies(1, observations)
	if len(anomalies) != 1 || anomalies[0].StartedAt != 300 || anomalies[0].EndedAt != 500 ||
		anomalies[0].PeakUsedPercent != 71 || anomalies[0].ObservationCount != 2 {
		t.Fatalf("real alternate regime was hidden by reset jitter: %+v", anomalies)
	}
	var points []quotaPoint
	for _, observation := range observations {
		points = append(points, quotaPoint{
			Time: observation.RequestedAt, ResetAt: observation.ResetAt,
			WindowMinutes: observation.WindowMinutes, UsedPercent: observation.UsedPercent,
		})
	}
	markQuotaAnomalyPoints(points, anomalies)
	if !points[2].Anomalous || !points[3].Anomalous || !points[2].BreakBefore ||
		points[4].Anomalous || !points[4].BreakBefore {
		t.Fatalf("real anomaly markers did not tolerate jitter: %+v", points)
	}
}

func TestResetJitterKeepsHistoricalForecastAndLearningSegments(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "reset-jitter.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	for index, delta := range []int64{0, 0, 60, 60, 0, 0} {
		used := float64(10 + index)
		at := int64(100 + index*100)
		if err = s.insertEvent(ctx, event{
			RequestedAt: at, ObservedAt: at, Account: "jitter", Model: "gpt-5.6-sol",
			InputTokens: 100, TotalTokens: 100, CostUSD: 1, UsedPercent: &used,
			ResetAt: 10000 + delta, WindowMinutes: 10080,
		}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	cycles, err := s.cycles(ctx, "jitter", 10)
	if err != nil || len(cycles) != 1 {
		t.Fatalf("jitter split a cycle: cycles=%+v err=%v", cycles, err)
	}
	points, _, err := s.pointsForCycle(ctx, "jitter", cycles[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		if point.Anomalous || point.BreakBefore {
			t.Fatalf("normal historical point was excluded: %+v", point)
		}
	}
	estimate := estimateCapacity(points)
	if !estimate.Available || estimate.SampleCount != 5 || estimate.FullWindowTokens != 10000 {
		t.Fatalf("normal history lost forecast evidence: %+v", estimate)
	}
	segments, err := s.quotaSegments(ctx, 0)
	if err != nil || len(segments) != 5 {
		t.Fatalf("learning segments=%+v err=%v", segments, err)
	}
	for _, segment := range segments {
		if !segment.eligible() {
			t.Fatalf("normal learning interval was excluded: %+v", segment)
		}
	}
}

func quotaRawEvidenceFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for _, table := range []string{"usage_events", "quota_samples", "quota_cycles"} {
		rows, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err = rows.Scan(destinations...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if err = encoder.Encode(values); err != nil {
				rows.Close()
				t.Fatal(err)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func TestQuotaAnomalyUpgradeRebuildsDerivedFlagsWithoutChangingRawEvidence(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "anomaly-upgrade.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	if _, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes,ended_at)
		VALUES(1,'a',100,10000,10080,10000);
		INSERT INTO usage_events(cycle_id,requested_at,observed_at,account,model,input_tokens,total_tokens,
			used_percent,reset_at,window_minutes,quota_scope) VALUES
		(1,100,100,'a','gpt-5.6-sol',100,100,10,10000,10080,'main'),
		(1,200,200,'a','gpt-5.6-sol',100,100,11,10000,10080,'main'),
		(1,300,300,'a','gpt-5.6-sol',100,100,12,10060,10080,'main'),
		(1,400,400,'a','gpt-5.6-sol',100,100,13,10060,10080,'main'),
		(1,500,500,'a','gpt-5.6-sol',100,100,14,10000,10080,'main'),
		(1,600,600,'a','gpt-5.6-sol',100,100,15,10000,10080,'main');
		INSERT INTO quota_samples(cycle_id,account,sampled_at,used_percent,reset_at,window_minutes)
		VALUES(1,'a',100,10,10000,10080),(1,'a',600,15,10000,10080);`); err != nil {
		t.Fatal(err)
	}
	if err = s.rebuildAllQuotaSegments(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE quota_segments SET flags_json='["quota_anomaly"]';
		DELETE FROM metadata WHERE key LIKE 'quota_segments_%';
		INSERT INTO metadata(key,value) VALUES('quota_segments_v2','complete')`); err != nil {
		t.Fatal(err)
	}
	before := quotaRawEvidenceFingerprint(t, s.db)
	if err = s.migrateSegments(); err != nil {
		t.Fatal(err)
	}
	segments, err := s.quotaSegments(ctx, 0)
	if err != nil || len(segments) != 5 {
		t.Fatalf("rebuilt segments=%+v err=%v", segments, err)
	}
	for _, segment := range segments {
		if !segment.eligible() {
			t.Fatalf("old anomaly flag survived upgrade: %+v", segment)
		}
	}
	if after := quotaRawEvidenceFingerprint(t, s.db); after != before {
		t.Fatalf("upgrade changed raw evidence: %s -> %s", before, after)
	}
	var maxIDBefore, maxIDAfter int64
	if err = s.db.QueryRow(`SELECT MAX(id) FROM quota_segments`).Scan(&maxIDBefore); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateSegments(); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT MAX(id) FROM quota_segments`).Scan(&maxIDAfter); err != nil || maxIDAfter != maxIDBefore {
		t.Fatalf("completed upgrade ran again: ids=%d->%d err=%v", maxIDBefore, maxIDAfter, err)
	}
}

func TestQuotaAnomalySnapshot(t *testing.T) {
	path := os.Getenv("QUOTA_ANOMALY_SNAPSHOT")
	if path == "" {
		t.Skip("set QUOTA_ANOMALY_SNAPSHOT to inspect a production backup read-only")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s := &store{db: db}
	ctx := context.Background()
	rows, err := db.Query(`SELECT id,started_at FROM quota_cycles ORDER BY started_at DESC,id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	var cycles []quotaCycle
	for rows.Next() {
		var cycle quotaCycle
		if err = rows.Scan(&cycle.ID, &cycle.StartedAt); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		cycles = append(cycles, cycle)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for index, cycle := range cycles {
		anomalies, err := s.quotaRegimeAnomalies(ctx, cycle.ID)
		if err != nil {
			t.Fatal(err)
		}
		var events, samples int64
		if err = db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE cycle_id=?`, cycle.ID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow(`SELECT COUNT(*) FROM quota_samples WHERE cycle_id=?`, cycle.ID).Scan(&samples); err != nil {
			t.Fatal(err)
		}
		t.Logf("history_index=%d cycle=%d start=%s events=%d samples=%d anomalies=%d",
			index, cycle.ID, time.Unix(cycle.StartedAt, 0).UTC().Format(time.RFC3339), events, samples, len(anomalies))
		total += len(anomalies)
	}
	t.Logf("cycles=%d confirmed_anomalies=%d", len(cycles), total)
}

func TestQuotaAnomalySnapshotUpgrade(t *testing.T) {
	path := os.Getenv("QUOTA_ANOMALY_UPGRADE_SNAPSHOT")
	if path == "" {
		t.Skip("set QUOTA_ANOMALY_UPGRADE_SNAPSHOT to replay an upgrade on a private backup copy")
	}
	source, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	source.SetMaxOpenConns(1)
	before := quotaRawEvidenceFingerprint(t, source)
	var oldFlags int64
	if err = source.QueryRow(`SELECT COUNT(*) FROM quota_segments WHERE flags_json LIKE '%quota_anomaly%'`).Scan(&oldFlags); err != nil {
		t.Fatal(err)
	}
	// VACUUM INTO copies committed SQLite state without modifying the source.
	copyPath := filepath.Join(t.TempDir(), "upgrade-copy.sqlite")
	if _, err = source.Exec(`VACUUM INTO ?`, copyPath); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	s, err := openStore(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	upgradeDuration := time.Since(started)
	if after := quotaRawEvidenceFingerprint(t, s.db); after != before {
		t.Fatalf("backup upgrade changed raw requests, samples or cycles: %s -> %s", before, after)
	}
	var flags, events, samples, cycleCount int64
	for query, destination := range map[string]*int64{
		`SELECT COUNT(*) FROM quota_segments WHERE flags_json LIKE '%quota_anomaly%'`: &flags,
		`SELECT COUNT(*) FROM usage_events`:                                           &events,
		`SELECT COUNT(*) FROM quota_samples`:                                          &samples,
		`SELECT COUNT(*) FROM quota_cycles`:                                           &cycleCount,
	} {
		if err = s.db.QueryRow(query).Scan(destination); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("upgrade_duration=%s anomaly_segment_flags=%d->%d raw_events=%d quota_samples=%d cycles=%d raw_fingerprint=%s",
		upgradeDuration, oldFlags, flags, events, samples, cycleCount, before)
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
			points, _, err := s.pointsForCycle(ctx, account, cycle.ID, 10000)
			if err != nil {
				t.Fatal(err)
			}
			estimate := estimateCapacity(points)
			t.Logf("history_index=%d cycle=%d points=%d forecast_available=%t forecast_samples=%d",
				index, cycle.ID, len(points), estimate.Available, estimate.SampleCount)
		}
	}
	cfg := defaultConfig()
	cfg.PriceCatalog, err = loadPriceCatalog(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.loadPricingSettings(ctx, cfg.pricingSettings())
	if err != nil {
		t.Fatal(err)
	}
	cfg = cfg.withPricingSettings(settings)
	previous, ok, err := s.latestWeightFit(ctx)
	if err != nil || !ok {
		t.Fatalf("previous fit available=%t err=%v", ok, err)
	}
	segments, err := s.quotaSegments(ctx, previous.SelectedLag)
	if err != nil {
		t.Fatal(err)
	}
	fit, err := fitQuotaWeights(segments, cfg.PriceCatalog, time.Now().Unix(), cfg.weightLearnerOptions())
	if err != nil || !fit.Available {
		t.Fatalf("rebuilt fit available=%t err=%v", fit.Available, err)
	}
	for _, pool := range fit.PooledModels {
		if pool.Model == legacyWeightReferenceModel {
			t.Logf("former_reference_scope=%s types=%v applied=%t value=%g fisher=%g reason=%s validation=%d",
				pool.Scope, pool.TokenTypes, pool.Applied, pool.Estimate.Value,
				pool.Diagnostic.ConditionalFisher, pool.Evidence.Reason, pool.Evidence.ValidationSegments)
		}
	}
	t.Logf("rebuilt_fit_reference=%s eligibility=%d segments=%d lag=%d",
		fit.ReferenceModel, fit.EligibilityVersion, fit.SegmentCount, fit.Lag)
}
