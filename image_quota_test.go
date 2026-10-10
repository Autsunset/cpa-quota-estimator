package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestQuotaScopeUsesActiveLimitWithoutReclassifyingLegacyImages(t *testing.T) {
	cases := []struct {
		name, model, alias, active, want string
		window                           int64
	}{
		{"image pool", "gpt-image-2", "", "imagegen_premium", imageQuotaScope, 1440},
		{"header beats model", "new-model", "", " IMAGEGEN_PREMIUM ", imageQuotaScope, 300},
		{"header beats alias", "new-model", "codex-spark", "imagegen", imageQuotaScope, 1440},
		{"legacy weekly image", "gpt-image-2", "", "", mainQuotaScope, 10080},
		{"explicit main pool", "gpt-image-2", "", "premium", mainQuotaScope, 1440},
		{"missing captured headers", "gpt-image-2.5", "", "", imageQuotaScope, 1440},
		{"image alias", "renamed-image", "gpt-image-2", "", imageQuotaScope, 1440},
		{"daily text is not image", "gpt-6.1-sol", "", "", mainQuotaScope, 1440},
		{"unknown pool stays main", "gpt-6.1-sol", "", "imagegenerator", mainQuotaScope, 1440},
		{"spark unchanged", "gpt-5.3-codex-spark", "", "spark", sparkQuotaScope, 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaScopeForObservation(tc.model, tc.alias, tc.active, tc.window); got != tc.want {
				t.Fatalf("scope=%q want %q", got, tc.want)
			}
		})
	}
}

func imageQuotaUsage(at int64, model, active string, used float64, reset, window int64) usageRecord {
	return usageRecord{
		Provider: "codex", Model: model, Alias: model, AuthID: "image-incident",
		RequestedAt: time.Unix(at, 0), Detail: usageDetail{InputTokens: 1000, TotalTokens: 1000},
		ResponseHeaders: http.Header{
			"X-Codex-Active-Limit":           {active},
			"X-Codex-Plan-Type":              {"pro"},
			"X-Codex-Primary-Used-Percent":   {strconv.FormatFloat(used, 'f', -1, 64)},
			"X-Codex-Primary-Reset-At":       {strconv.FormatInt(reset, 10)},
			"X-Codex-Primary-Window-Minutes": {strconv.FormatInt(window, 10)},
		},
	}
}

func TestImageQuotaDoesNotChangeMainSeriesOrAccounting(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(strconv.FormatBool(capture), func(t *testing.T) {
			s, err := openStore(filepath.Join(t.TempDir(), "live.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.close()
			cfg := defaultConfig()
			cfg.CaptureCodexHeaders = capture
			a := &app{cfg: cfg, store: s}
			records := []usageRecord{
				imageQuotaUsage(1791636380, "gpt-6.1-sol", "premium", 80, 1791949566, 10080),
				imageQuotaUsage(1791636529, "gpt-image-2", "imagegen_premium", 0, 1791722930, 1440),
				imageQuotaUsage(1791636709, "gpt-6.1-sol", "premium", 80, 1791949566, 10080),
				imageQuotaUsage(1791636900, "gpt-image-2", "imagegen_premium", 0, 1791722930, 1440),
				imageQuotaUsage(1791637015, "gpt-6.1-sol", "premium", 80, 1791949566, 10080),
				imageQuotaUsage(1791637122, "gpt-6.1-sol", "premium", 81, 1791949566, 10080),
			}
			for _, r := range records {
				if err = a.recordUsage(r); err != nil {
					t.Fatal(err)
				}
				cycles, err := s.cycles(context.Background(), r.AuthID, 10)
				if err != nil || len(cycles) != 1 {
					t.Fatalf("cycles=%+v err=%v", cycles, err)
				}
				if cycles[0].WindowMinutes != 10080 || cycles[0].ResetAt != 1791949560 {
					t.Fatalf("image changed main cycle: %+v", cycles[0])
				}
			}
			resp := a.handleManagement(managementRequest{Method: "GET", Path: "/cpa-quota-estimator/series", Query: url.Values{"account": {records[0].AuthID}}})
			if resp.StatusCode != 200 {
				t.Fatalf("series status=%d body=%s", resp.StatusCode, resp.Body)
			}
			var series struct {
				Points    []quotaPoint         `json:"points"`
				Anomalies []quotaRegimeAnomaly `json:"quota_anomalies"`
			}
			if err = json.Unmarshal(resp.Body, &series); err != nil {
				t.Fatal(err)
			}
			for _, p := range series.Points {
				if p.WindowMinutes != 10080 || p.UsedPercent < 80 || p.Anomalous {
					t.Fatalf("image leaked into main series: %+v", p)
				}
			}
			last := series.Points[len(series.Points)-1]
			if last.UsedPercent != 81 || last.Requests != 4 || last.WindowTokens != 4000 || len(series.Anomalies) != 0 {
				t.Fatalf("latest=%+v anomalies=%v", last, series.Anomalies)
			}
			var raw, images int
			if err = s.db.QueryRow(`SELECT COUNT(*),SUM(quota_scope='image' AND cycle_id=0) FROM usage_events`).Scan(&raw, &images); err != nil || raw != 6 || images != 2 {
				t.Fatalf("raw=%d images=%d err=%v", raw, images, err)
			}
			monthly, err := s.monthly(context.Background(), records[0].AuthID, "2026-10")
			if err != nil || monthly.Requests != 4 || monthly.ActualTokens != 4000 {
				t.Fatalf("monthly=%+v err=%v", monthly, err)
			}
		})
	}
}

func TestImageQuotaAloneNeverCreatesMainCycleOrLearnerSegments(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "image-only.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}
	for i := int64(0); i < 3; i++ {
		r := imageQuotaUsage(1791636529+i*120, "gpt-image-2", "imagegen_premium", float64(i), 1791722930, 1440)
		if err = a.recordUsage(r); err != nil {
			t.Fatal(err)
		}
	}
	var cycles, segments, progress int
	for table, target := range map[string]*int{"quota_cycles": &cycles, "quota_segments": &segments, "quota_segment_progress": &progress} {
		if err = s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if cycles != 0 || segments != 0 || progress != 0 {
		t.Fatalf("cycles=%d segments=%d progress=%d", cycles, segments, progress)
	}
	if got := learnedQuotaAttribution(&weightFit{}, "a", imageQuotaScope, "gpt-image-2", "", 0, 0, usageDetail{}, 272000); got != nil {
		t.Fatalf("image attribution=%v", got)
	}
	if got := s.learnedQuotaAttributionLive(context.Background(), &weightFit{}, "a", imageQuotaScope, "gpt-image-2", "", 0, usageDetail{}, 272000); got != nil {
		t.Fatalf("live image attribution=%v", got)
	}
}

func TestImageQuotaDoesNotCreateSecondaryWeeklyLedger(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "image-secondary.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := &app{cfg: defaultConfig(), store: s}
	r := imageQuotaUsage(1791636529, "new-image-model", "imagegen_premium", 0, 1791654540, 300)
	r.ResponseHeaders.Set("X-Codex-Secondary-Used-Percent", "40")
	r.ResponseHeaders.Set("X-Codex-Secondary-Reset-At", "1791949560")
	r.ResponseHeaders.Set("X-Codex-Secondary-Window-Minutes", "10080")
	if err = a.recordUsage(r); err != nil {
		t.Fatal(err)
	}
	var scope string
	var cycle, secondaryReset int64
	var secondaryUsed sql.NullFloat64
	if err = s.db.QueryRow(`SELECT quota_scope,cycle_id,secondary_used_percent,secondary_reset_at FROM usage_events`).Scan(&scope, &cycle, &secondaryUsed, &secondaryReset); err != nil {
		t.Fatal(err)
	}
	if scope != imageQuotaScope || cycle != 0 || secondaryUsed.Valid || secondaryReset != 0 {
		t.Fatalf("scope=%s cycle=%d secondary=%+v reset=%d", scope, cycle, secondaryUsed, secondaryReset)
	}
}

func seedLegacyImageIncident(t *testing.T, s *store, rawHeaders string, imageFirst bool) {
	t.Helper()
	at, mainReset, imageReset := int64(1791636380), int64(1791949560), int64(1791722940)
	records := []event{
		{RequestedAt: at, Model: "gpt-6.1-sol", UsedPercent: floatPointer(80), ResetAt: mainReset, WindowMinutes: 10080, TotalTokens: 1000, CostUSD: 2},
		{RequestedAt: at + 120, Model: "gpt-image-2", UsedPercent: floatPointer(0), ResetAt: imageReset, WindowMinutes: 1440, TotalTokens: 50, CostUSD: 3, CodexHeadersJSON: rawHeaders},
		{RequestedAt: at + 300, Model: "gpt-6.1-sol", UsedPercent: floatPointer(81), ResetAt: mainReset, WindowMinutes: 10080, TotalTokens: 2000, CostUSD: 4},
		{RequestedAt: at + 480, Model: "gpt-image-2", UsedPercent: floatPointer(1), ResetAt: imageReset, WindowMinutes: 1440, TotalTokens: 60, CostUSD: 5, CodexHeadersJSON: rawHeaders},
	}
	if imageFirst {
		records = records[1:]
	}
	for _, e := range records {
		e.Account, e.Provider, e.PlanType, e.QuotaScope = "image-incident", "codex", "pro", mainQuotaScope
		if err := s.insertEvent(context.Background(), e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM metadata WHERE key=?`, imageQuotaRepairKey); err != nil {
		t.Fatal(err)
	}
}

func floatPointer(v float64) *float64 { return &v }

func TestImageQuotaUpgradeRepairsHistoryAndIsIdempotent(t *testing.T) {
	for _, raw := range []string{`{"X-Codex-Active-Limit":["imagegen_premium"]}`, "", "{malformed"} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.sqlite")
			s, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			seedLegacyImageIncident(t, s, raw, false)
			s.close()
			s, err = openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.close() }()
			cycles, err := s.cycles(context.Background(), "image-incident", 10)
			if err != nil || len(cycles) != 1 {
				t.Fatalf("cycles=%+v err=%v", cycles, err)
			}
			cycle := cycles[0]
			if cycle.WindowMinutes != 10080 || cycle.ResetAt != 1791949560 || cycle.EndPercent != 81 || cycle.Requests != 2 || cycle.ActualTokens != 3000 || cycle.ActualCostUSD != 6 {
				t.Fatalf("repaired cycle=%+v", cycle)
			}
			var count, images int
			var tokens int64
			var cost float64
			if err = s.db.QueryRow(`SELECT COUNT(*),SUM(total_tokens),SUM(cost_usd),SUM(quota_scope='image' AND cycle_id=0) FROM usage_events`).Scan(&count, &tokens, &cost, &images); err != nil || count != 4 || tokens != 3110 || cost != 14 || images != 2 {
				t.Fatalf("raw count=%d tokens=%d cost=%g images=%d err=%v", count, tokens, cost, images, err)
			}
			points, _, err := s.pointsForCycle(context.Background(), "image-incident", cycle.ID, 5000)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range points {
				if p.WindowMinutes != 10080 || p.UsedPercent < 80 || p.Requests > 2 || p.WindowTokens > 3000 {
					t.Fatalf("unrepaired point=%+v", p)
				}
			}
			segments, err := s.quotaSegments(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, seg := range segments {
				if seg.RegimeResetAt != 1791949560 {
					t.Fatalf("image regime retained: %+v", seg)
				}
			}
			if err = s.repairImageQuotaScopes(context.Background()); err != nil {
				t.Fatal(err)
			}
			again, _, err := s.pointsForCycle(context.Background(), "image-incident", cycle.ID, 5000)
			if err != nil || len(again) != len(points) {
				t.Fatalf("repeat repair points=%d want %d err=%v", len(again), len(points), err)
			}
		})
	}
}

func TestImageQuotaUpgradePreservesExplicitMainAndWeeklyImageUsage(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "compatibility.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for i, spec := range []struct {
		window int64
		raw    string
	}{{10080, ""}, {1440, `{"X-Codex-Active-Limit":["premium"]}`}} {
		e := event{Account: strconv.Itoa(i), Model: "gpt-image-2", QuotaScope: mainQuotaScope, RequestedAt: 1791636380, UsedPercent: floatPointer(20), ResetAt: 1791949560, WindowMinutes: spec.window, CodexHeadersJSON: spec.raw}
		if err = s.insertEvent(context.Background(), e, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec(`DELETE FROM metadata WHERE key=?`, imageQuotaRepairKey); err != nil {
		t.Fatal(err)
	}
	if err = s.repairImageQuotaScopes(context.Background()); err != nil {
		t.Fatal(err)
	}
	var main int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE quota_scope='main' AND cycle_id>0`).Scan(&main); err != nil || main != 2 {
		t.Fatalf("legacy main images=%d err=%v", main, err)
	}
}

func TestImageQuotaUpgradeRollsBackOnFailure(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "rollback.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	seedLegacyImageIncident(t, s, "", false)
	if _, err = s.db.Exec(`CREATE TRIGGER reject_image_scope BEFORE UPDATE OF quota_scope ON usage_events
WHEN NEW.quota_scope='image' AND OLD.id=(SELECT MAX(id) FROM usage_events)
BEGIN SELECT RAISE(ABORT,'injected repair failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.repairImageQuotaScopes(context.Background()); err == nil {
		t.Fatal("expected transaction failure")
	}
	var main, samples int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE quota_scope='main' AND cycle_id>0`).Scan(&main); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples`).Scan(&samples); err != nil || main != 4 || samples != 4 {
		t.Fatalf("rollback main=%d samples=%d err=%v", main, samples, err)
	}
	var marker string
	if err = s.db.QueryRow(`SELECT value FROM metadata WHERE key=?`, imageQuotaRepairKey).Scan(&marker); err != sql.ErrNoRows {
		t.Fatalf("repair marker=%q err=%v", marker, err)
	}
}

func TestImageQuotaUpgradeFixesImageFirstCycleStart(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "image-first.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	seedLegacyImageIncident(t, s, "", true)
	if err = s.repairImageQuotaScopes(context.Background()); err != nil {
		t.Fatal(err)
	}
	cycles, err := s.cycles(context.Background(), "image-incident", 10)
	if err != nil || len(cycles) != 1 || cycles[0].StartedAt != 1791949560-10080*60 {
		t.Fatalf("cycles=%+v err=%v", cycles, err)
	}
}

func TestImageQuotaUpgradeRemovesImageOnlyLegacyCycle(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "image-only-legacy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	e := event{Account: "image-incident", Model: "gpt-image-2", QuotaScope: mainQuotaScope, RequestedAt: 1791636529, UsedPercent: floatPointer(0), ResetAt: 1791722940, WindowMinutes: 1440, TotalTokens: 1000}
	if err = s.insertEvent(context.Background(), e, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM metadata WHERE key=?`, imageQuotaRepairKey); err != nil {
		t.Fatal(err)
	}
	if err = s.repairImageQuotaScopes(context.Background()); err != nil {
		t.Fatal(err)
	}
	var cycles, samples, images int
	if err = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM quota_cycles),(SELECT COUNT(*) FROM quota_samples),
(SELECT COUNT(*) FROM usage_events WHERE quota_scope='image' AND cycle_id=0 AND total_tokens=1000)`).Scan(&cycles, &samples, &images); err != nil || cycles != 0 || samples != 0 || images != 1 {
		t.Fatalf("cycles=%d samples=%d images=%d err=%v", cycles, samples, images, err)
	}
}

func TestImageQuotaUpgradeKeepsCorroboratedMainSample(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "coincident.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	e := event{Account: "coincident", Model: "text", QuotaScope: mainQuotaScope, RequestedAt: 1791636529, UsedPercent: floatPointer(20), ResetAt: 1791722940, WindowMinutes: 1440, TotalTokens: 1000}
	if err = s.insertEvent(context.Background(), e, time.Minute); err != nil {
		t.Fatal(err)
	}
	e.Model, e.TotalTokens = "gpt-image-2", 50
	if err = s.insertEvent(context.Background(), e, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DELETE FROM metadata WHERE key=?`, imageQuotaRepairKey); err != nil {
		t.Fatal(err)
	}
	if err = s.repairImageQuotaScopes(context.Background()); err != nil {
		t.Fatal(err)
	}
	var samples, requests int
	var tokens int64
	if err = s.db.QueryRow(`SELECT COUNT(*),MAX(requests),MAX(window_tokens) FROM quota_samples`).Scan(&samples, &requests, &tokens); err != nil || samples != 1 || requests != 1 || tokens != 1000 {
		t.Fatalf("samples=%d requests=%d tokens=%d err=%v", samples, requests, tokens, err)
	}
}

func TestImageQuotaServerSnapshotUpgrade(t *testing.T) {
	snapshot := os.Getenv("IMAGE_QUOTA_REVIEW_SNAPSHOT")
	if snapshot == "" {
		t.Skip("set IMAGE_QUOTA_REVIEW_SNAPSHOT to an online SQLite backup")
	}
	source, err := os.Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	path := filepath.Join(t.TempDir(), "review.sqlite")
	target, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy=%v close=%v", copyErr, closeErr)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var beforeCount, beforeTokens, beforeSamples int64
	var beforeCost float64
	if err = db.QueryRow(`SELECT COUNT(*),SUM(total_tokens),SUM(cost_usd) FROM usage_events`).Scan(&beforeCount, &beforeTokens, &beforeCost); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM quota_samples`).Scan(&beforeSamples); err != nil {
		t.Fatal(err)
	}
	db.Close()
	started := time.Now()
	s, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var count, tokens, samples, images, badImages int64
	var cost float64
	if err = s.db.QueryRow(`SELECT COUNT(*),SUM(total_tokens),SUM(cost_usd),SUM(quota_scope='image'),
SUM(quota_scope='main' AND codex_headers_json LIKE '%imagegen_premium%') FROM usage_events`).Scan(&count, &tokens, &cost, &images, &badImages); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM quota_samples`).Scan(&samples); err != nil {
		t.Fatal(err)
	}
	if count != beforeCount || tokens != beforeTokens || cost != beforeCost || images == 0 || badImages != 0 || samples >= beforeSamples {
		t.Fatalf("counts=%d/%d tokens=%d/%d cost=%g/%g images=%d bad=%d samples=%d/%d", count, beforeCount, tokens, beforeTokens, cost, beforeCost, images, badImages, samples, beforeSamples)
	}
	var integrity string
	if err = s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%s err=%v", integrity, err)
	}
	t.Logf("upgrade=%s raw_events=%d preserved_tokens=%d preserved_value=%g images=%d removed_samples=%d", time.Since(started), count, tokens, cost, images, beforeSamples-samples)
}
