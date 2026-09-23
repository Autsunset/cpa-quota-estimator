package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildQuotaSegmentsCrossingsAndLag(t *testing.T) {
	events := []segmentEvent{
		{ID: 1, RequestedAt: 100, Model: "gpt-5.6-sol", InputTokens: 100, HasUsed: true, UsedPercent: 0, ResetAt: 1000},
		{ID: 2, RequestedAt: 110, Model: "gpt-5.6-sol", InputTokens: 200, HasUsed: true, UsedPercent: 0, ResetAt: 1000},
		{ID: 3, RequestedAt: 120, Model: "gpt-5.6-sol", InputTokens: 300, HasUsed: true, UsedPercent: 1, ResetAt: 1000},
		{ID: 4, RequestedAt: 130, Model: "gpt-5.6-sol", InputTokens: 400, HasUsed: true, UsedPercent: 1, ResetAt: 1000},
		{ID: 5, RequestedAt: 140, Model: "gpt-5.6-sol", InputTokens: 500, HasUsed: true, UsedPercent: 2, ResetAt: 1000},
	}
	key := segmentKey{Account: "a", Window: mainQuotaScope, CycleID: 1}
	known := map[string]bool{"gpt-5.6-sol": true}
	for lag, want := range map[int][]int64{0: {500, 900}, 1: {300, 700}, 2: {100, 500}} {
		segments := buildQuotaSegments(key, events, lag, known, 272000, nil)
		if len(segments) != 2 {
			t.Fatalf("lag %d segments = %d", lag, len(segments))
		}
		for i, segment := range segments {
			if segment.StartEventID != events[i*2].ID || segment.EndEventID != events[i*2+2].ID || segment.DP != 1 {
				t.Fatalf("lag %d segment %d boundaries = %#v", lag, i, segment)
			}
			if lag == 2 && i == 0 {
				if len(segment.Flags) != 1 || segment.Flags[0] != "lag_incomplete" {
					t.Fatalf("lag 2 first segment flags = %v", segment.Flags)
				}
				continue
			}
			if len(segment.Features) != 1 || segment.Features[0].Tokens != want[i] {
				t.Fatalf("lag %d segment %d features = %#v, want %d", lag, i, segment.Features, want[i])
			}
		}
	}
}

func TestBuildQuotaSegmentsFlagsBadIntervals(t *testing.T) {
	for _, status := range []int{0, 408, 499, 502} {
		if !isInterruptedStatus(status) {
			t.Fatalf("status %d not interrupted", status)
		}
	}
	if isInterruptedStatus(503) {
		t.Fatal("503 must remain a separate failure")
	}
	events := []segmentEvent{
		{ID: 1, RequestedAt: 100, Model: "gpt-5.6-sol", HasUsed: true, UsedPercent: 0, ResetAt: 1000},
		{ID: 2, RequestedAt: 110, Model: "unknown-model", InputTokens: 100, Failed: true, HasUsed: true, UsedPercent: 0, ResetAt: 1000},
		{ID: 3, RequestedAt: 120, Model: "gpt-5.6-sol", InputTokens: 100, HasUsed: true, UsedPercent: 2, ResetAt: 1000},
		{ID: 4, RequestedAt: 130, Model: "gpt-5.6-sol", HasUsed: true, UsedPercent: 0, ResetAt: 1000},
		{ID: 5, RequestedAt: 140, Model: "gpt-5.6-sol", InputTokens: 100, HasUsed: true, UsedPercent: 3, ResetAt: 1200},
	}
	segments := buildQuotaSegments(segmentKey{Account: "a", Window: mainQuotaScope, CycleID: 1}, events, 0,
		map[string]bool{"gpt-5.6-sol": true}, 272000, nil)
	if len(segments) != 2 {
		t.Fatal(segments)
	}
	for _, flag := range []string{"failed_request", "unknown_model"} {
		if !hasSegmentFlag(segments[0].Flags, flag) {
			t.Fatalf("first segment missing %s: %v", flag, segments[0].Flags)
		}
	}
	if segments[0].InterruptedCount != 1 || segments[0].OtherFailedCount != 0 {
		t.Fatalf("interrupt counts=%#v", segments[0])
	}
	for _, flag := range []string{"quota_drop", "regime_change"} {
		if !hasSegmentFlag(segments[1].Flags, flag) {
			t.Fatalf("second segment missing %s: %v", flag, segments[1].Flags)
		}
	}
	if segments[0].eligible() || segments[1].eligible() {
		t.Fatal("bad segments were marked eligible")
	}
}

func hasSegmentFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if flag == want {
			return true
		}
	}
	return false
}

func TestQuotaSegmentHistoricalBackfill(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "segments.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if _, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes) VALUES(1,'a',100,1000,15);
	INSERT INTO usage_events(cycle_id,requested_at,observed_at,account,model,input_tokens,total_tokens,used_percent,reset_at,window_minutes,quota_scope) VALUES
	(1,100,100,'a','gpt-5.6-sol',100,100,0,1000,15,'main'),
	(1,110,110,'a','gpt-5.6-sol',200,200,0,1000,15,'main'),
	(1,120,120,'a','gpt-5.6-sol',300,300,1,1000,15,'main');`); err != nil {
		t.Fatal(err)
	}
	if err = s.rebuildAllQuotaSegments(context.Background()); err != nil {
		t.Fatal(err)
	}
	segments, err := s.quotaSegments(context.Background(), 0)
	if err != nil || len(segments) != 1 {
		t.Fatalf("segments = %#v, err = %v", segments, err)
	}
	if segments[0].DP != 1 || segments[0].Features[0].Tokens != 500 {
		t.Fatal(segments[0])
	}
	var flags string
	if err = s.db.QueryRow(`SELECT flags_json FROM quota_segments WHERE lag=0`).Scan(&flags); err != nil {
		t.Fatal(err)
	}
	var decoded []string
	if err = json.Unmarshal([]byte(flags), &decoded); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaSegmentsRefreshOnNewCrossing(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "incremental.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	for index, used := range []float64{0, 0, 1, 1, 2} {
		value := used
		e := event{RequestedAt: int64(100 + index*10), ObservedAt: int64(100 + index*10),
			Account: "a", Model: "gpt-5.6-sol", InputTokens: 100, TotalTokens: 100,
			UsedPercent: &value, ResetAt: 1000, WindowMinutes: 15, QuotaScope: mainQuotaScope}
		if err = s.insertEvent(context.Background(), e, 5*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	segments, err := s.quotaSegments(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].DP != 1 || segments[1].DP != 1 {
		t.Fatalf("incremental segments = %#v", segments)
	}
}

func TestWeeklySegmentIncludesRequestsWithoutSecondaryHeader(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "weekly.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if _, err = s.db.Exec(`INSERT INTO usage_events(requested_at,observed_at,account,model,input_tokens,total_tokens,quota_scope,
		used_percent,reset_at,window_minutes,secondary_used_percent,secondary_reset_at,secondary_window_minutes) VALUES
		(100,100,'a','gpt-5.6-sol',100,100,'main',0,1900,300,0,604800,10080),
		(110,110,'a','gpt-5.6-sol',200,200,'main',0,1900,300,NULL,0,0),
		(120,120,'a','gpt-5.6-sol',300,300,'main',1,1900,300,1,604800,10080);`); err != nil {
		t.Fatal(err)
	}
	if err = s.rebuildAllQuotaSegments(context.Background()); err != nil {
		t.Fatal(err)
	}
	segments, err := s.quotaSegments(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var weekly *quotaSegment
	for i := range segments {
		if segments[i].Window == weeklyQuotaScope {
			weekly = &segments[i]
		}
	}
	if weekly == nil || len(weekly.Features) != 1 || weekly.Features[0].Tokens != 500 {
		t.Fatalf("weekly features = %#v", weekly)
	}
}

func TestHistoricalBackfillSplitsHiddenResetAndMergesMinuteJitter(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "hidden-reset.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if _, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes) VALUES(4,'a',100,300000,10080);
	INSERT INTO usage_events(cycle_id,requested_at,observed_at,account,model,input_tokens,total_tokens,used_percent,reset_at,window_minutes,quota_scope) VALUES
	(4,100,100,'a','gpt-5.6-sol',100,100,0,1000,10080,'main'),
	(4,110,110,'a','gpt-5.6-sol',100,100,1,1060,10080,'main'),
	(4,200,200,'a','gpt-5.6-sol',100,100,0,200000,10080,'main'),
	(4,210,210,'a','gpt-5.6-sol',100,100,1,200060,10080,'main');`); err != nil {
		t.Fatal(err)
	}
	if err = s.rebuildAllQuotaSegments(context.Background()); err != nil {
		t.Fatal(err)
	}
	segments, err := s.quotaSegments(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].RegimeResetAt == segments[1].RegimeResetAt || segments[0].DP != 1 || segments[1].DP != 1 {
		t.Fatalf("hidden reset segments=%#v", segments)
	}
}
