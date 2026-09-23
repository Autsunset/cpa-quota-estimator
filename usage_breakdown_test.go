package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
)

func TestUsageBreakdownSeparatesOfficialCreditsFromAccountQuota(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if _, err = s.db.Exec(`INSERT INTO quota_cycles(id,account,started_at,reset_at,window_minutes) VALUES(1,'a',100,1000,15);
INSERT INTO quota_samples(cycle_id,sampled_at,account,used_percent,reset_at,window_minutes) VALUES
(1,150,'a',0,1000,15),(1,450,'a',3,1000,15);
INSERT INTO usage_events(cycle_id,requested_at,account,model,service_tier,input_tokens,cache_read_tokens,output_tokens,total_tokens,cost_usd,failed,quota_scope) VALUES
(1,200,'a','gpt-5.6-sol','auto',1000000,500000,100000,1100000,5,0,'main'),
(1,300,'a','openai/gpt-6-sol','priority',1000000,0,0,1000000,2,0,'main'),
(1,400,'a','gpt-6-luna','auto',1000000,0,0,1000000,0.1,0,'main'),
(1,500,'a','gemini-3.7-flash','auto',0,0,0,0,0,1,'main'),
(1,500,'a','gpt-5.6-sol','auto',1000000,0,0,1000000,100,0,'spark');`); err != nil {
		t.Fatal(err)
	}
	got, err := s.usageBreakdown(context.Background(), "a", 100, 600, 7, defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 4 || got.Failed != 1 || got.UnpricedRequests != 1 || got.TotalTokens != 3_100_000 {
		t.Fatalf("totals = %#v", got)
	}
	if math.Abs(got.OfficialCredits-232.5) > 1e-9 || got.QuotaGrowthPercent != 3 || !got.QuotaCoverageComplete {
		t.Fatalf("credits or account quota = %#v", got)
	}
	byModel := make(map[string]usageBreakdownRow)
	for _, row := range got.Rows {
		byModel[row.Model] = row
	}
	for model, want := range map[string]float64{"gpt-5.6-sol": 105, "gpt-6-sol": 125, "gpt-6-luna": 2.5} {
		row := byModel[model]
		if row.OfficialCredits == nil || math.Abs(*row.OfficialCredits-want) > 1e-9 {
			t.Fatalf("%s row = %#v, want %g credits", model, row, want)
		}
	}
	if byModel["gemini-3.7-flash"].OfficialCredits != nil {
		t.Fatal("unpublished model must not receive an invented official credit price")
	}
}
