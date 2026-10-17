package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestGroupedIdentifiabilityPreservesCycleScanResults(t *testing.T) {
	const now = int64(1800000000)
	segments := referenceSyntheticSegments(false)
	for i := range segments {
		segments[i].CycleID = int64(i/30 + 1)
		segments[i].BoundaryWeight = .2 + float64(i%5)/5
		if i%3 == 0 {
			segments[i].Features = append(segments[i].Features, segmentFeature{
				Model: "gpt-6-astra", Type: "output", Tokens: int64(10000 * (1 + i%7)), Fast: i%2 == 0,
			})
		}
	}
	opts := legacyWeightLearnerOptions()
	for _, model := range []string{legacyWeightReferenceModel, "gpt-6-astra", "unobserved"} {
		match := func(f segmentFeature) bool { return f.Model == model && f.Type == "output" }
		got := assessWeightParameter(segments, nil, now, opts, model, match, opts.ModelShareMinSD, opts.ModelMinFisher)
		// Retain the old independent cycle scan as a numerical regression oracle.
		cycles := map[string][]quotaSegment{}
		for _, segment := range segments {
			cycles[modelCycle(segment)] = append(cycles[modelCycle(segment)], segment)
		}
		var variance, weight, fisher float64
		for _, group := range cycles {
			var ratios []float64
			for _, segment := range group {
				ratios = append(ratios, segment.DP/legacyReferenceEquivalent(segment, pricingModeCredits, nil))
			}
			sort.Float64s(ratios)
			scale := ratios[len(ratios)/2]
			var sums fisherCycleSums
			for _, segment := range group {
				total := legacyReferenceEquivalent(segment, pricingModeCredits, nil)
				var target float64
				for _, feature := range segment.Features {
					if match(feature) {
						target += float64(feature.Tokens) / 1e6 * referenceFeatureRate(feature, pricingModeCredits, nil) / legacyReferenceInputRate(pricingModeCredits, nil)
					}
				}
				w := segmentFitWeight(segment, now, opts.HalfLifeDays)
				sums.Weight += w
				sums.Share += w * target / total
				sums.ShareSquared += w * (target / total) * (target / total)
				sums.TotalSquared += w * (scale * total) * (scale * total)
				sums.Cross += w * (scale * total) * (scale * target)
				sums.TargetSquared += w * (scale * target) * (scale * target)
			}
			if sums.TargetSquared > 0 {
				variance += sums.ShareSquared - sums.Share*sums.Share/sums.Weight
				weight += sums.Weight
			}
			fisher += (sums.TargetSquared - sums.Cross*sums.Cross/sums.TotalSquared) / (weightObservationSD * weightObservationSD)
		}
		shareSD := 0.0
		if weight > 0 {
			shareSD = math.Sqrt(math.Max(0, variance/weight))
		}
		fisher = math.Max(0, fisher)
		if math.Abs(got.WithinCycleShareSD-shareSD) > 1e-10 ||
			math.Abs(got.ConditionalFisher-fisher) > 1e-9*math.Max(1, fisher) ||
			got.Unlocked != (shareSD >= opts.ModelShareMinSD && fisher >= opts.ModelMinFisher) {
			t.Fatalf("%s grouped=%+v scan_share_sd=%g scan_fisher=%g", model, got, shareSD, fisher)
		}
	}
}

func TestRollingBacktestReportsFitsAndHonorsCancellation(t *testing.T) {
	for _, count := range []int{0, 24, 25, 49, 80} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			byLag := map[int][]quotaSegment{}
			for lag := 0; lag <= 2; lag++ {
				byLag[lag] = referenceSyntheticSegments(false)[:count]
			}
			last, total, calls := -1, 0, 0
			_, err := rollingBacktestProgress(byLag, nil, 1800000000, legacyWeightLearnerOptions(), func(done, target int) {
				if done < last || target < 1 || (total != 0 && target != total) || done > target {
					t.Fatalf("invalid progress %d/%d after %d/%d", done, target, last, total)
				}
				last, total = done, target
				calls++
			})
			want := 1
			if count >= 25 {
				start := max(25, count*40/100)
				want += 3 * (1 + max(0, count-start-1)/25)
			}
			if err != nil || total != want || last != total || calls != total+1 {
				t.Fatalf("progress=%d/%d calls=%d want=%d err=%v", last, total, calls, want, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := legacyWeightLearnerOptions()
	opts.ctx = ctx
	segments := referenceSyntheticSegments(false)[:80]
	_, err := rollingBacktestProgress(map[int][]quotaSegment{0: segments, 1: segments, 2: segments}, nil, 1800000000, opts, func(done, total int) {
		if done == 1 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CPU fitting ignored cancellation: %v", err)
	}
	if _, err = fitQuotaWeights(segments, nil, 1800000000, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled direct fit returned %v", err)
	}
}

func pricingEvidenceFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	hash := sha256.New()
	for _, table := range []string{"usage_events", "quota_samples", "quota_cycles"} {
		rows, err := db.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatal(err)
		}
		var columns []string
		for rows.Next() {
			var id, notNull, primary int
			var name, typ string
			var fallback any
			if err = rows.Scan(&id, &name, &typ, &notNull, &fallback, &primary); err != nil {
				t.Fatal(err)
			}
			if name != "cost_usd" && name != "window_cost_usd" && name != "learned_quota_pct" && name != "learned_secondary_pct" {
				columns = append(columns, `"`+name+`"`)
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		rows, err = db.Query("SELECT " + strings.Join(columns, ",") + " FROM " + table + " ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(hash, table)
		values, pointers := make([]any, len(columns)), make([]any, len(columns))
		for i := range pointers {
			pointers[i] = &values[i]
		}
		for rows.Next() {
			if err = rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			if err = json.NewEncoder(hash).Encode(values); err != nil {
				t.Fatal(err)
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func TestPricingReferenceRecalculationSnapshot(t *testing.T) {
	sourcePath := os.Getenv("PRICING_REFERENCE_SNAPSHOT")
	if sourcePath == "" {
		t.Skip("set PRICING_REFERENCE_SNAPSHOT to replay an online backup offline")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(t.TempDir(), "pricing-reference.sqlite")
	target, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if err = errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
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
	previous, ok, err := s.latestWeightFitForReference(ctx, cfg.anchorModel())
	if err != nil || !ok {
		t.Fatalf("previous fit available=%t err=%v", ok, err)
	}
	cfg.LearnedFit = &previous.FittedWeights
	a := &app{store: s, cfg: cfg}
	settings.AnchorModel = legacyWeightReferenceModel
	if reference := os.Getenv("SELECTED_REFERENCE_MODEL"); reference != "" {
		settings.AnchorModel = reference
	}
	before := pricingEvidenceFingerprint(t, s.db)
	started := time.Now()
	task, err := a.startPricingTask(s, settings, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var lastStage string
	lastFits := -1
	deadline := time.After(10 * time.Minute)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for task.Status != "succeeded" && task.Status != "failed" {
		select {
		case <-deadline:
			t.Fatal("reference pricing task did not finish in ten minutes")
		case <-ticker.C:
			task, _ = a.latestPricingTask(task.ID)
			if task.Progress.Stage != lastStage || task.Progress.FitsDone > lastFits {
				t.Logf("elapsed=%s status=%s progress=%+v", time.Since(started).Round(time.Millisecond), task.Status, task.Progress)
				lastStage, lastFits = task.Progress.Stage, task.Progress.FitsDone
			}
		}
	}
	if task.Status != "succeeded" {
		t.Fatalf("repricing failed: %+v", task)
	}
	a.mu.RLock()
	updated := a.cfg
	a.mu.RUnlock()
	if !updated.fitMatchesReference(updated.LearnedFit) || updated.anchorModel() != settings.AnchorModel {
		t.Fatal("task did not install a fit in the selected reference unit")
	}
	if after := pricingEvidenceFingerprint(t, s.db); after != before {
		t.Fatalf("repricing changed raw evidence: %s -> %s", before, after)
	}
	settings, err = s.loadPricingSettings(ctx, cfg.pricingSettings())
	if err != nil || settings.AnchorModel != updated.anchorModel() {
		t.Fatalf("reference was not persisted: %+v, %v", settings, err)
	}
	rows, err := s.db.Query(`SELECT cycle_id,sampled_at,window_cost_usd FROM quota_samples WHERE id%101=0 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type check struct {
		cycle, at int64
		cost      float64
	}
	var samples []check
	for rows.Next() {
		var sample check
		if err = rows.Scan(&sample.cycle, &sample.at, &sample.cost); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, sample)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, sample := range samples {
		var sum float64
		if err = s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM usage_events WHERE cycle_id=? AND quota_scope='main' AND requested_at<=?`, sample.cycle, sample.at).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		if math.Abs(sum-sample.cost) > 1e-7*math.Max(1, math.Abs(sum)) {
			t.Fatalf("sample cost=%g raw sum=%g", sample.cost, sum)
		}
	}
	t.Logf("completed=%s events=%d segments=%d reference=%s samples_checked=%d raw_fingerprint=%s",
		time.Since(started).Round(time.Millisecond), task.RecalculatedEvents, updated.LearnedFit.SegmentCount,
		updated.anchorModel(), len(samples), before)
	if path := os.Getenv("PRICING_REFERENCE_OUTPUT"); path != "" {
		result, ok, readErr := s.latestWeightFitForReference(ctx, updated.anchorModel())
		if readErr != nil || !ok {
			t.Fatalf("updated fit available=%t err=%v", ok, readErr)
		}
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err = os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
