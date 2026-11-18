package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDecodeCodexCreditsRateCard(t *testing.T) {
	rates, err := decodeCodexCredits(`
## Speed multipliers
Fast

| Model | Included usage | Credits / PAYG |
| GPT-6.2 Astra<br /> GPT-6.2 Sol | 2x | 2x |
| GPT-7 Sol | 2.5x | 1.5x |

Ultrafast
| GPT-6.2 Astra | 6x | 3x |

## Token rates
<table><tbody>
<tr><td><a href="#astra">GPT-6.2 Astra</a></td><td>200 credits</td><td>10 credits</td><td>1,000 credits</td></tr>
<tr><td>GPT-Image-2 (text)</td><td>125 credits</td><td>31.25 credits</td><td>250 credits</td></tr>
</tbody><tfoot><tr><td colspan="4">Not a rate</td></tr></tfoot></table>

| GPT-6.2 Sol | 50 | 2.5 | 250 |
| GPT-7 Sol | 80 | 4 | 400 |
| GPT-7 Astra | 500 | -12 | 850 |

## Other usage
| GPT-8 Sol | 10 | 2 | 7 |
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 3 || rates["gpt-6.2-astra"] != (codexCreditRates{200, 10, 1000, 2}) ||
		rates["gpt-6.2-sol"] != (codexCreditRates{50, 2.5, 250, 2}) ||
		rates["gpt-7-sol"] != (codexCreditRates{80, 4, 400, 1.5}) {
		t.Fatalf("unexpected rate card: %+v", rates)
	}
	if _, err := decodeCodexCredits("## Error\nService unavailable"); err == nil {
		t.Fatal("accepted an invalid rate card")
	}
}

func TestPriceSyncRepricesPreviouslyUnpricedModelAndRetainsLastGoodRates(t *testing.T) {
	ctx := context.Background()
	var phase atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api":
			if phase.Load() == 0 {
				fmt.Fprint(w, `{"providers":{"openai":{"models":{"gpt-6.2-astra":{"cost":{"input":8,"output":40}}}}}}`)
			} else {
				fmt.Fprint(w, `{"providers":{"openai":{"models":{"gpt-6.2-astra":{"cost":{"input":7,"output":35}},"gpt-6-chat-thinking":{"cost":{"input":1,"output":4}}}}}}`)
			}
		case "/credits":
			if phase.Load() == 0 {
				fmt.Fprint(w, "## Speed multipliers\n| GPT-6 Chat Thinking | 2x | 1.5x |\n## Token rates\n| GPT-6 Chat Thinking | 30 | 1.5 | 120 |\n")
			} else {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}
		default:
			http.NotFound(w, req)
		}
	}))
	defer upstream.Close()
	cfg := defaultConfig()
	cfg.DataPath = filepath.Join(t.TempDir(), "prices.sqlite")
	cfg.PriceSourceURL = upstream.URL + "/api"
	cfg.CodexPriceSourceURL = upstream.URL + "/credits"
	s, err := openStore(cfg.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{store: s, cfg: cfg, priceSyncWake: make(chan struct{}, 1)}
	defer a.shutdown()
	if err = seedPrices(ctx, s); err != nil {
		t.Fatal(err)
	}
	const model = "gpt-6-chat-thinking"
	p, found, err := s.getPrice(ctx, model)
	if err != nil || !found || cfg.priceRow(p).Available {
		t.Fatalf("thinking should have an unpriced catalog entry: %+v %t %v", p, found, err)
	}
	if err = a.recordUsage(usageRecord{AuthID: "test", Model: model, ServiceTier: "fast", Detail: usageDetail{InputTokens: 1_000_000}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.priceSyncWake:
	default:
		t.Fatal("unpriced usage did not request a background refresh")
	}
	readCost := func() float64 {
		t.Helper()
		var cost float64
		if err := s.db.QueryRowContext(ctx, `SELECT cost_usd FROM usage_events WHERE model=?`, model).Scan(&cost); err != nil {
			t.Fatal(err)
		}
		return cost
	}
	if cost := readCost(); cost != 0 {
		t.Fatalf("unpriced request cost = %g", cost)
	}
	sync := func() priceSyncResult {
		t.Helper()
		result, err := a.syncPriceCatalog(ctx, s, cfg)
		if err != nil {
			t.Fatal(err)
		}
		task, ok := a.latestPricingTask("")
		if !ok {
			t.Fatal("catalog update did not schedule repricing")
		}
		if task = waitPricingTask(t, a, task.ID); task.Status != "succeeded" {
			t.Fatalf("repricing failed: %+v", task)
		}
		return result
	}
	if result := sync(); result.Count != 2 || len(result.Warnings) != 0 {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	if cost := readCost(); cost != 45 {
		t.Fatalf("repriced request should use the published Fast rate: got %g", cost)
	}
	now := time.Now().Unix()
	data, err := s.usageBreakdown(ctx, "test", now-60, now+60, 0, a.cfg)
	if err != nil || data.OfficialCredits != 45 || data.UnpricedRequests != 0 {
		t.Fatalf("usage breakdown did not use the synced rate: %+v %v", data, err)
	}
	phase.Store(1)
	if result := sync(); result.Count != 2 || result.CodexCount != 0 || len(result.Warnings) != 1 {
		t.Fatalf("unexpected partial sync result: %+v", result)
	}
	if revision, err := s.pendingPriceCatalogRevision(ctx); err != nil || revision != 0 {
		t.Fatalf("successful reprice left pending catalog revision %d: %v", revision, err)
	}
	lastTask, _ := a.latestPricingTask("")
	sync()
	unchanged, _ := a.latestPricingTask("")
	if unchanged.ID != lastTask.ID {
		t.Fatal("an unchanged catalog started redundant repricing")
	}
	if err := a.quiesce(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(cfg.DataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if err = seedPrices(ctx, reopened); err != nil {
		t.Fatal(err)
	}
	p, found, err = reopened.getPrice(ctx, model)
	if err != nil || !found || p.Input != 1 || p.Codex != (codexCreditRates{30, 1.5, 120, 1.5}) {
		t.Fatalf("API sync or cached credits lost on reopen: %+v %t %v", p, found, err)
	}
	p, _, err = reopened.getPrice(ctx, "gpt-6.2-astra")
	if err != nil || p.Input != 7 || p.Output != 35 {
		t.Fatalf("new API prices overwritten by built-in rates: %+v %v", p, err)
	}
}
