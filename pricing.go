package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type catalogProvider struct {
	Models map[string]json.RawMessage `json:"models"`
}

func syncPrices(ctx context.Context, s *store, cfg config) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.PriceSourceURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", pluginID+"/"+pluginVersion)
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return 0, fmt.Errorf("price source: %s", res.Status)
	}
	prices, err := decodeCatalog(res.Body)
	if err != nil {
		return 0, err
	}
	if err = s.upsertPrices(ctx, prices); err != nil {
		return 0, err
	}
	return len(prices), nil
}

func decodeCatalog(r io.Reader) ([]price, error) {
	var root struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.NewDecoder(r).Decode(&root); err != nil {
		return nil, err
	}
	if len(root.Providers) == 0 {
		return nil, fmt.Errorf("models.dev catalog has no providers")
	}
	var provider catalogProvider
	var raw json.RawMessage
	for id, v := range root.Providers {
		if strings.EqualFold(id, "openai") {
			raw = v
			break
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("models.dev catalog has no openai provider")
	}
	if err := json.Unmarshal(raw, &provider); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	out := make([]price, 0, len(provider.Models))
	for model, rawModel := range provider.Models {
		var entry struct {
			Cost struct {
				Input      float64 `json:"input"`
				Output     float64 `json:"output"`
				CacheRead  float64 `json:"cache_read"`
				CacheWrite float64 `json:"cache_write"`
				Tiers      []struct {
					Input      float64 `json:"input"`
					Output     float64 `json:"output"`
					CacheRead  float64 `json:"cache_read"`
					CacheWrite float64 `json:"cache_write"`
					Tier       struct {
						Type string `json:"type"`
						Size int64  `json:"size"`
					} `json:"tier"`
				} `json:"tiers"`
			} `json:"cost"`
			Experimental struct {
				Modes map[string]struct {
					Cost struct {
						Input      float64 `json:"input"`
						Output     float64 `json:"output"`
						CacheRead  float64 `json:"cache_read"`
						CacheWrite float64 `json:"cache_write"`
					} `json:"cost"`
				} `json:"modes"`
			} `json:"experimental"`
		}
		if json.Unmarshal(rawModel, &entry) != nil {
			continue
		}
		if entry.Cost.Input == 0 && entry.Cost.Output == 0 && entry.Cost.CacheRead == 0 && entry.Cost.CacheWrite == 0 {
			continue
		}
		p := price{Model: normalizeModel(model), Input: entry.Cost.Input, Output: entry.Cost.Output, CacheRead: entry.Cost.CacheRead, CacheWrite: entry.Cost.CacheWrite, Source: "models.dev/openai", UpdatedAt: now}
		for _, tier := range entry.Cost.Tiers {
			if strings.EqualFold(tier.Tier.Type, "context") {
				p.LongInput = tier.Input
				p.LongOutput = tier.Output
				p.LongRead = tier.CacheRead
				p.LongWrite = tier.CacheWrite
				break
			}
		}
		if fast, ok := entry.Experimental.Modes["fast"]; ok {
			p.FastInput = fast.Cost.Input
			p.FastOutput = fast.Cost.Output
			p.FastRead = fast.Cost.CacheRead
			p.FastWrite = fast.Cost.CacheWrite
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models.dev catalog has no usable OpenAI prices")
	}
	return out, nil
}

func seedPrices(ctx context.Context, s *store) error {
	now := time.Now().Unix()
	return s.upsertPrices(ctx, []price{
		{Model: "gpt-5.6-sol", Input: 5, Output: 30, CacheRead: .5, CacheWrite: 6.25, LongInput: 10, LongOutput: 45, LongRead: 1, LongWrite: 12.5, FastInput: 10, FastOutput: 60, FastRead: 1, FastWrite: 12.5, Source: "built-in fallback", UpdatedAt: now},
		{Model: "gpt-5.6-luna", Input: .2, Output: 1.2, CacheRead: .02, CacheWrite: .25, LongInput: .4, LongOutput: 1.8, LongRead: .04, LongWrite: .5, FastInput: .4, FastOutput: 2.4, FastRead: .04, FastWrite: .5, Source: "built-in fallback", UpdatedAt: now},
		{Model: "gpt-5.6-terra", Input: 2, Output: 12, CacheRead: .2, CacheWrite: 2.5, LongInput: 4, LongOutput: 18, LongRead: .4, LongWrite: 5, FastInput: 4, FastOutput: 24, FastRead: .4, FastWrite: 5, Source: "built-in fallback", UpdatedAt: now},
	})
}

func calculateCost(p price, d usageDetail, serviceTier string, cfg config) float64 {
	in, out, read, write := p.Input, p.Output, p.CacheRead, p.CacheWrite
	if cfg.ApplyLongContextPricing && d.InputTokens > cfg.LongContextThreshold && p.LongInput > 0 {
		in, out, read, write = p.LongInput, p.LongOutput, p.LongRead, p.LongWrite
	}
	if cfg.ApplyFastPricing && isFastTier(serviceTier) {
		if strings.EqualFold(cfg.FastPricingMode, "source") && p.FastInput > 0 {
			in, out, read, write = p.FastInput, p.FastOutput, p.FastRead, p.FastWrite
		} else {
			in *= cfg.FastMultiplier
			out *= cfg.FastMultiplier
			read *= cfg.FastMultiplier
			write *= cfg.FastMultiplier
		}
	}
	cacheRead := d.CacheReadTokens
	if d.CachedTokens > cacheRead {
		cacheRead = d.CachedTokens
	}
	cacheWrite := d.CacheCreationTokens
	uncached := d.InputTokens - cacheRead - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*in + float64(cacheRead)*read + float64(cacheWrite)*write + float64(d.OutputTokens)*out) / 1_000_000
}

func isFastTier(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "priority" || t == "fast"
}
