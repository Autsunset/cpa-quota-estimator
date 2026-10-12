package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func referenceContrastSegments(reference, target string, prices map[string]price, factor float64) []quotaSegment {
	var segments []quotaSegment
	for i := 0; i < 60; i++ {
		features := []segmentFeature{{Model: reference, Type: "input", Tokens: 2000000}}
		multiplier := 1.0
		if i%2 == 1 {
			features = []segmentFeature{
				{Model: target, Type: "input", Tokens: 1000000},
				{Model: target, Type: "cache", Tokens: 20000000},
				{Model: target, Type: "output", Tokens: 200000},
			}
			multiplier = factor
		}
		segment := quotaSegment{Account: "dynamic", Window: mainQuotaScope, CycleID: 1,
			RegimeResetAt: 1900000000, EndAt: 1800000000 + int64(i), BoundaryWeight: 1, Features: features}
		segment.DP = multiplier * referenceEquivalent(segment, pricingModeCredits, prices, reference)
		segments = append(segments, segment)
	}
	return segments
}

func TestSelectedReferenceReleasesFormerSolForPooledCalibration(t *testing.T) {
	opts := defaultWeightLearnerOptions()
	opts.ReferenceModel = "gpt-6-astra"
	segments := referenceContrastSegments(opts.ReferenceModel, legacyWeightReferenceModel, nil, 1.4)
	fit, err := fitQuotaWeights(segments, nil, 1800000100, opts)
	if err != nil || !fit.Available || fit.ReferenceModel != opts.ReferenceModel || fit.ReferenceInputCredits != 250 {
		t.Fatalf("selected reference fit=%+v err=%v", fit, err)
	}
	var reference, target *learnedModelWeights
	for i := range fit.Models {
		switch fit.Models[i].Model {
		case opts.ReferenceModel:
			reference = &fit.Models[i]
		case legacyWeightReferenceModel:
			target = &fit.Models[i]
		}
	}
	if reference == nil || reference.Input.Source != "anchor" || reference.Input.Value != 1 ||
		reference.Input.Identified || reference.Input.Low != 1 || reference.Input.High != 1 {
		t.Fatalf("selected input did not become the only gauge: %+v", reference)
	}
	if target == nil || target.Input.Source != "pooled" || target.Input.Identified {
		t.Fatalf("former reference did not become a normal pooled target: %+v", target)
	}
	var applied bool
	for _, pool := range fit.PooledModels {
		if pool.Model == legacyWeightReferenceModel {
			applied = pool.Applied && pool.Evidence.Accepted && math.Abs(pool.Estimate.Value-1.4) < .1
		}
		if pool.Model == opts.ReferenceModel {
			t.Fatal("the reference input gauge became a free pooled coefficient")
		}
	}
	if !applied {
		t.Fatalf("former reference pooling was not validated: %+v", fit.PooledModels)
	}
	cfg := defaultConfig()
	cfg.AnchorModel, cfg.LearnedFit = opts.ReferenceModel, &fit
	p, _ := officialGPT6Price(legacyWeightReferenceModel)
	row := cfg.priceRow(p)
	if row.Adjustment.Source != "pooled" || row.Adjustment.Calibrated || math.Abs(row.Calculated.Input-140) > 10 {
		t.Fatalf("former reference still has special inverse-anchor status: %+v", row)
	}
	for _, segment := range segments {
		scale, _ := learnedScale(&fit, segment.Account, segment.Window, segment.CycleID, segment.RegimeResetAt)
		if math.Abs(fit.predict(segment, nil)-learnedSegmentEquivalent(segment, fit)*scale) > 1e-5 {
			t.Fatal("serialized prediction and request accounting disagree on reference units")
		}
	}
}

func TestReferenceWorksWithoutSolOrAnyBuiltInModel(t *testing.T) {
	prices := map[string]price{
		"future-anchor": {Model: "future-anchor", Input: 8, CacheRead: .8, Output: 40},
		"future-model":  {Model: "future-model", Input: 4, CacheRead: .4, Output: 20},
	}
	opts := defaultWeightLearnerOptions()
	opts.ReferenceModel = "future-anchor"
	segments := referenceContrastSegments(opts.ReferenceModel, "future-model", prices, 1.4)
	fit, err := fitQuotaWeights(segments, prices, 1800000100, opts)
	if err != nil || !fit.Available || fit.ReferenceModel != opts.ReferenceModel || fit.ReferenceInputCredits != 200 {
		t.Fatalf("catalog-only models still depend on a built-in model: fit=%+v err=%v", fit, err)
	}
	if len(fit.Models) != 2 {
		t.Fatalf("unused hardcoded models leaked into fit: %+v", fit.Models)
	}
	for _, row := range fit.Models {
		if row.Model == legacyWeightReferenceModel {
			t.Fatal("a removed model was required or inserted as the fit reference")
		}
		if row.Model == opts.ReferenceModel && (row.Input.Source != "anchor" || row.Input.Value != 1) {
			t.Fatalf("catalog reference was not fixed at one: %+v", row)
		}
	}
	cfg := defaultConfig()
	cfg.AnchorModel, cfg.PriceCatalog, cfg.LearnedFit = opts.ReferenceModel, prices, &fit
	row := cfg.priceRow(prices["future-model"])
	if row.PooledEstimate == nil || !row.PooledEstimate.Applied || row.Calculated.Input <= row.Official.Input {
		t.Fatalf("catalog-only prices did not apply the learned factor: %+v", row)
	}
	scores, err := prequentialScores(segments, prices, opts)
	if err != nil || len(scores) != 3 || scores[2].SegmentCount == 0 {
		t.Fatalf("backtesting still depends on Sol: scores=%+v err=%v", scores, err)
	}
	opts.ReferenceModel = "not-in-catalog"
	if _, err := fitQuotaWeights(segments, prices, 1800000100, opts); err == nil {
		t.Fatal("an unknown reference silently fell back to a hardcoded model")
	}
}

func TestSelectedReferenceCacheAndOutputRemainLearnable(t *testing.T) {
	segments := referenceSyntheticSegments(false)
	for i := range segments {
		for j := range segments[i].Features {
			segments[i].Features[j].Model = "gpt-6-astra"
		}
	}
	opts := defaultWeightLearnerOptions()
	opts.ReferenceModel = "gpt-6-astra"
	fit, err := fitQuotaWeights(segments, nil, 1800000000, opts)
	if err != nil || !fit.Available {
		t.Fatalf("fit unavailable: %v", err)
	}
	for _, row := range fit.Models {
		if row.Model == opts.ReferenceModel {
			if row.Input.Value != 1 || row.Input.Identified || !supportedWeight(row.Cache) ||
				!supportedWeight(row.Output) || math.Abs(row.Cache.Value/.16-1) > .08 || math.Abs(row.Output.Value/7-1) > .08 {
				t.Fatalf("reference convention locked cache/output evidence: %+v", row)
			}
			return
		}
	}
	t.Fatal("selected reference missing")
}

func TestOnlineScaleUnitsChangeWithReferenceAndRejectStaleFits(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "reference-online.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	old := weightFit{Available: true, ReferenceModel: legacyWeightReferenceModel, ReferenceInputCredits: 100, FittedAt: 10,
		CycleScales: []learnedCycleScale{{Account: "old", Window: mainQuotaScope, CycleID: 1, RegimeResetAt: 100,
			Scale: weightEstimate{Value: .5, Low: .4, High: .6}}}}
	if err = s.seedOnlineCycleScales(ctx, &old); err != nil {
		t.Fatal(err)
	}
	next := weightFit{Available: true, ReferenceModel: "future-anchor", ReferenceInputCredits: 200, FittedAt: 20,
		CycleScales: []learnedCycleScale{{Account: "new", Window: mainQuotaScope, CycleID: 2, RegimeResetAt: 200,
			Scale: weightEstimate{Value: .8, Low: .7, High: .9}}}}
	if err = s.seedOnlineCycleScales(ctx, &next); err != nil {
		t.Fatal(err)
	}
	var oldRows int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM online_cycle_scales WHERE account='old'`).Scan(&oldRows); err != nil || oldRows != 0 {
		t.Fatalf("old-unit online scales survived reference change: rows=%d err=%v", oldRows, err)
	}
	current, err := s.overlayOnlineCycleScales(ctx, next)
	if err != nil || math.Abs(current.CycleScales[0].CreditsPerPercent.Value-250) > 1e-9 {
		t.Fatalf("capacity conversion still assumes 100-credit Sol input: %+v err=%v", current, err)
	}
	stale, err := s.overlayOnlineCycleScales(ctx, old)
	if err != nil || len(stale.CycleScales) != 1 || stale.CycleScales[0].Account != "old" {
		t.Fatalf("new-unit scales leaked into a historical fit: %+v err=%v", stale, err)
	}
	a := &app{store: s, cfg: defaultConfig()}
	a.cfg.AnchorModel, a.cfg.LearnedFit = next.ReferenceModel, &next
	a.applyFittedWeights(ctx, s, &old)
	if a.cfg.LearnedFit != &next {
		t.Fatal("a stale background fit overwrote the selected reference")
	}
}

func TestFitLookupAndRefreshRespectReferenceMetadata(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "reference-fit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	for _, reference := range []string{"", "gpt-6-astra", "future-anchor", legacyWeightReferenceModel} {
		credits := 100.0
		if reference == "gpt-6-astra" {
			credits = 250
		} else if reference == "future-anchor" {
			credits = 200
		}
		result := weightBacktest{GeneratedAt: time.Now().Unix(), FittedWeights: weightFit{
			ReferenceModel: reference, ReferenceInputCredits: credits, EligibilityVersion: weightEligibilityVersion, FittedAt: time.Now().Unix(),
			HalfLifeDays: 21, RandomWalkSigma: .35}}
		if _, err = s.saveWeightFit(ctx, result, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	for _, reference := range []string{legacyWeightReferenceModel, "gpt-6-astra", "future-anchor"} {
		result, ok, err := s.latestWeightFitForReference(ctx, reference)
		if err != nil || !ok || fitReferenceModel(&result.FittedWeights) != reference {
			t.Fatalf("lookup mixed reference bases: fit=%+v ok=%t err=%v", result, ok, err)
		}
		cfg := defaultConfig()
		cfg.AnchorModel = reference
		cfg.PriceCatalog = map[string]price{"future-anchor": {Model: "future-anchor", Input: 8}}
		if fitNeedsRefresh(result, true, cfg) {
			t.Fatal("a current fit with matching reference was rejected")
		}
		stale := result
		stale.FittedWeights.ReferenceInputCredits *= 2
		if !fitNeedsRefresh(stale, true, cfg) {
			t.Fatal("a changed input-unit price did not invalidate the cached fit")
		}
		cfg.AnchorModel = "different-anchor"
		if !fitNeedsRefresh(result, true, cfg) {
			t.Fatal("reference changes did not invalidate old fitted diagnostics")
		}
	}
	a := &app{store: s, cfg: defaultConfig()}
	a.cfg.AnchorModel = "future-anchor"
	response := a.handleManagement(managementRequest{Method: "GET", Path: "/weights"})
	var fit weightFit
	if response.StatusCode != 200 || json.Unmarshal(response.Body, &fit) != nil || fit.ReferenceModel != "future-anchor" {
		t.Fatalf("weights endpoint exposed another anchor's fit: %d %s", response.StatusCode, response.Body)
	}
}

func TestAnchorSwitchRefitsCatalogOnlyModelsAndRollsBackUnitsOnFailure(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "reference-task.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	ctx := context.Background()
	prices := map[string]price{
		"future-anchor": {Model: "future-anchor", Input: 8, CacheRead: .8, Output: 40},
		"future-model":  {Model: "future-model", Input: 4, CacheRead: .4, Output: 20},
	}
	for _, p := range prices {
		if err = s.upsertPrices(ctx, []price{p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes)
		VALUES(1,'dynamic',1800000000,1900000000,10080)`); err != nil {
		t.Fatal(err)
	}
	for i, segment := range referenceContrastSegments("future-anchor", "future-model", prices, 1.4) {
		if _, err = s.db.Exec(`INSERT INTO usage_events(cycle_id,requested_at,account,model,input_tokens,total_tokens,cost_usd,quota_scope)
			VALUES(1,?,'dynamic',?,1000000,1000000,99,'main')`, segment.EndAt, segment.Features[0].Model); err != nil {
			t.Fatal(err)
		}
		features, _ := json.Marshal(segment.Features)
		for lag := 0; lag <= 2; lag++ {
			if _, err = s.db.Exec(`INSERT INTO quota_segments(account,window,cycle_id,regime_reset_at,lag,
				start_event_id,end_event_id,start_at,end_at,dp,boundary_weight,features_json,flags_json,created_at)
				VALUES('dynamic','main',1,1900000000,?,?,?,?,?,?,1,?,'[]',?)`,
				lag, i, i+1, segment.EndAt-1, segment.EndAt, segment.DP, string(features), segment.EndAt); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := defaultConfig()
	cfg.PriceCatalog = prices
	a := &app{store: s, cfg: cfg}
	save := func(reference string) pricingRecalcTask {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"anchor_model": reference})
		response := a.handleManagement(managementRequest{Method: "POST", Path: "/pricing-settings", Body: body})
		var task pricingRecalcTask
		if response.StatusCode != 202 || json.Unmarshal(response.Body, &task) != nil {
			t.Fatalf("save failed: %d %s", response.StatusCode, response.Body)
		}
		return waitPricingTask(t, a, task.ID)
	}
	if task := save("future-anchor"); task.Status != "succeeded" {
		t.Fatalf("catalog-only reference fitting failed: %+v", task)
	}
	for _, reference := range []string{"future-model", "future-anchor"} {
		if task := save(reference); task.Status != "succeeded" {
			t.Fatalf("reference switch failed: %+v", task)
		}
		if a.cfg.LearnedFit == nil || !a.cfg.LearnedFit.Available || a.cfg.LearnedFit.ReferenceModel != reference {
			t.Fatalf("reference switch reused an incompatible fit: %+v", a.cfg.LearnedFit)
		}
		for _, row := range a.cfg.LearnedFit.Models {
			if row.Model == reference && (row.Input.Source != "anchor" || row.Input.Value != 1) {
				t.Fatalf("selected input is not one: %+v", row)
			}
		}
	}
	priorFit := a.cfg.LearnedFit
	a.pricingProgressHook = func(progress pricingRecalcProgress) error {
		if progress.Stage == "events" {
			return errors.New("injected post-fit repricing failure")
		}
		return nil
	}
	if task := save("future-model"); task.Status != "failed" {
		t.Fatalf("injected failure was not reported: %+v", task)
	}
	if a.cfg.anchorModel() != "future-anchor" || a.cfg.LearnedFit != priorFit {
		t.Fatal("failed repricing did not restore the previous reference and fit")
	}
	if matches, err := s.onlineWeightBasisMatches(ctx, priorFit); err != nil || !matches {
		t.Fatalf("failed repricing left online scales in the new unit: matches=%t err=%v", matches, err)
	}
	settings, err := s.loadPricingSettings(ctx, a.cfg.pricingSettings())
	if err != nil || settings.AnchorModel != "future-anchor" {
		t.Fatalf("persisted settings diverged after rollback: %+v err=%v", settings, err)
	}
	var events, segments, tokens int64
	if err = s.db.QueryRow(`SELECT COUNT(*),SUM(total_tokens) FROM usage_events`).Scan(&events, &tokens); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_segments`).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if events != 60 || segments != 180 || tokens != 60000000 {
		t.Fatalf("refitting altered raw evidence: events=%d segments=%d tokens=%d", events, segments, tokens)
	}
}

func TestSelectedReferenceSnapshot(t *testing.T) {
	path := os.Getenv("SELECTED_REFERENCE_SNAPSHOT")
	if path == "" {
		t.Skip("set SELECTED_REFERENCE_SNAPSHOT to validate an online backup read-only")
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	s := &store{db: db}
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
	previous, ok, err := s.latestWeightFit(ctx)
	if err != nil || !ok {
		t.Fatalf("previous fit available=%t err=%v", ok, err)
	}
	segments, err := s.quotaSegments(ctx, previous.SelectedLag)
	if err != nil {
		t.Fatal(err)
	}
	fit, err := fitQuotaWeights(segments, cfg.PriceCatalog, time.Now().Unix(), cfg.weightLearnerOptions())
	if err != nil || !fit.Available || !cfg.fitMatchesReference(&fit) {
		t.Fatalf("selected-reference fit available=%t reference=%s parameters=%d err=%v",
			fit.Available, fit.ReferenceModel, len(fit.ParameterNames), err)
	}
	cfg.LearnedFit = &fit
	var found bool
	for _, row := range fit.Models {
		if row.Model == cfg.anchorModel() {
			found = true
			if row.Input.Source != "anchor" || row.Input.Value != 1 || row.Input.Low != 1 || row.Input.High != 1 || row.Input.Identified {
				t.Fatalf("selected input gauge=%+v", row.Input)
			}
		} else if row.Input.Source == "anchor" {
			t.Fatalf("another model retained a hardcoded input gauge: %s", row.Model)
		}
		t.Logf("model=%s input=%s cache=%s output=%s", row.Model, row.Input.Source, row.Cache.Source, row.Output.Source)
	}
	if !found {
		t.Fatal("selected reference missing from snapshot fit")
	}
	t.Logf("reference=%s input_credits=%g segments=%d parameters=%d lag=%d mae=%g",
		fit.ReferenceModel, fit.ReferenceInputCredits, fit.SegmentCount, len(fit.ParameterNames), fit.Lag, fit.MeanAbsError)
}
