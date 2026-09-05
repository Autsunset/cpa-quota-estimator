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

const (
	pricingModeCurrentAPI = "current_api"
	pricingModeLegacyAPI  = "legacy_api"
	pricingModeCredits    = "credits"
)

// modelPriceMultiplier is a quota-equivalence calibration, not an upstream
// price. Keep catalog prices unchanged so syncs and UI retain the base rates.
func modelPriceMultiplier(model string) float64 {
	if normalizeModel(model) == "gpt-6-astra" {
		return 1.8
	}
	return 1
}

func modelPriceMultipliers() map[string]float64 {
	return map[string]float64{"gpt-6-astra": modelPriceMultiplier("gpt-6-astra")}
}

func validPricingMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case pricingModeCurrentAPI, pricingModeLegacyAPI, pricingModeCredits:
		return true
	default:
		return false
	}
}

func normalizePricingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case pricingModeLegacyAPI:
		return pricingModeLegacyAPI
	case pricingModeCredits:
		return pricingModeCredits
	default:
		return pricingModeCurrentAPI
	}
}

func pricingValueUnit(mode string) string {
	if normalizePricingMode(mode) == pricingModeCredits {
		return "credits"
	}
	return "USD"
}

func priceForPricingMode(p price, mode string) price {
	mode = normalizePricingMode(mode)
	if mode == pricingModeCurrentAPI {
		return p
	}
	// Subscription Credits deliberately derive from the non-promotional
	// Codex rate card. Temporary purchased-credit/API discounts must never
	// leak into this mode.
	base := legacyAPIPrice(p)
	if mode != pricingModeCredits {
		return base
	}
	base.Input *= 25
	base.Output *= 25
	base.CacheRead *= 25
	base.CacheWrite = 0 // The subscription Credits rate card does not charge cache writes.
	base.LongInput *= 25
	base.LongOutput *= 25
	base.LongRead *= 25
	base.LongWrite = 0
	// Credits use the saved Fast multiplier rather than the current API
	// source-mode prices, which may contain temporary promotional rates.
	base.FastInput = 0
	base.FastOutput = 0
	base.FastRead = 0
	base.FastWrite = 0
	return base
}

func legacyAPIPrice(p price) price {
	switch normalizeModel(p.Model) {
	case "gpt-5.6", "gpt-5.6-sol":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 5, 30, .5, 6.25
		p.LongInput, p.LongOutput, p.LongRead, p.LongWrite = 10, 45, 1, 12.5
		p.FastInput, p.FastOutput, p.FastRead, p.FastWrite = 0, 0, 0, 0
	case "gpt-5.6-terra":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 2.5, 15, .25, 3.125
		p.LongInput, p.LongOutput, p.LongRead, p.LongWrite = 5, 22.5, .5, 6.25
		p.FastInput, p.FastOutput, p.FastRead, p.FastWrite = 0, 0, 0, 0
	case "gpt-5.6-luna":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 1, 6, .1, 1.25
		p.LongInput, p.LongOutput, p.LongRead, p.LongWrite = 2, 9, .2, 2.5
		p.FastInput, p.FastOutput, p.FastRead, p.FastWrite = 0, 0, 0, 0
	}
	return p
}

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
		{Model: "gpt-6-astra", Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5, LongInput: 20, LongOutput: 75, LongRead: 2, LongWrite: 25, FastInput: 20, FastOutput: 100, FastRead: 2, FastWrite: 25, Source: "built-in fallback", UpdatedAt: now},
		{Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: .4, CacheWrite: 5, LongInput: 8, LongOutput: 30, LongRead: .8, LongWrite: 10, FastInput: 8, FastOutput: 40, FastRead: .8, FastWrite: 10, Source: "built-in fallback", UpdatedAt: now},
		{Model: "gpt-5.6-luna", Input: .2, Output: 1.2, CacheRead: .02, CacheWrite: .25, LongInput: .4, LongOutput: 1.8, LongRead: .04, LongWrite: .5, FastInput: .4, FastOutput: 2.4, FastRead: .04, FastWrite: .5, Source: "built-in fallback", UpdatedAt: now},
		{Model: "gpt-5.6-terra", Input: 2, Output: 12, CacheRead: .2, CacheWrite: 2.5, LongInput: 4, LongOutput: 18, LongRead: .4, LongWrite: 5, FastInput: 4, FastOutput: 24, FastRead: .4, FastWrite: 5, Source: "built-in fallback", UpdatedAt: now},
	})
}

func calculateCost(p price, d usageDetail, serviceTier string, cfg config) float64 {
	p = priceForPricingMode(p, cfg.PricingMode)
	in, out, read, write := p.Input, p.Output, p.CacheRead, p.CacheWrite
	if cfg.ApplyLongContextPricing && d.InputTokens > cfg.LongContextThreshold && p.LongInput > 0 {
		in, out, read, write = p.LongInput, p.LongOutput, p.LongRead, p.LongWrite
	}
	if cfg.ApplyFastPricing && isFastTier(serviceTier) {
		if normalizePricingMode(cfg.PricingMode) == pricingModeCurrentAPI && strings.EqualFold(cfg.FastPricingMode, "source") && p.FastInput > 0 {
			in, out, read, write = p.FastInput, p.FastOutput, p.FastRead, p.FastWrite
		} else {
			in *= cfg.FastMultiplier
			out *= cfg.FastMultiplier
			read *= cfg.FastMultiplier
			write *= cfg.FastMultiplier
		}
	}
	cacheRead := max(d.CacheReadTokens, d.CachedTokens)
	cacheWrite := d.CacheCreationTokens
	uncached := d.InputTokens - cacheRead - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*in + float64(cacheRead)*read + float64(cacheWrite)*write + float64(d.OutputTokens)*out) / 1_000_000 * modelPriceMultiplier(p.Model)
}

func isFastTier(t string) bool {
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "priority" || t == "fast"
}
