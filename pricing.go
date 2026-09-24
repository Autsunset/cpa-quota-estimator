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
	pricingModeAPI    = "api"
	pricingModeCustom = "custom"
	// Old names are accepted at the API boundary only for migration.
	pricingModeCurrentAPI = "current_api"
	pricingModeLegacyAPI  = "legacy_api"
	pricingModeCredits    = "credits"
	pricingModeLearned    = "learned"
)

func validPricingMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case pricingModeAPI, pricingModeCredits, pricingModeCustom, pricingModeCurrentAPI, pricingModeLegacyAPI, pricingModeLearned:
		return true
	default:
		return false
	}
}

func normalizePricingMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case pricingModeAPI, pricingModeCurrentAPI, pricingModeLegacyAPI:
		return pricingModeAPI
	case pricingModeCredits:
		return pricingModeCredits
	case pricingModeCustom:
		return pricingModeCustom
	case pricingModeLearned:
		return pricingModeCredits
	default:
		return pricingModeCredits
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
	if mode == pricingModeAPI || mode == pricingModeCustom {
		return p
	}
	if mode == pricingModeCredits {
		if official, ok := officialCodexCreditPrice(p.Model); ok {
			return official
		}
		// Keep a clearly documented estimate for models without a published
		// Codex credit rate, so older history is not silently valued at zero.
		base := p
		base.Input *= 25
		base.Output *= 25
		base.CacheRead *= 25
		base.CacheWrite = 0
		base.LongInput *= 25
		base.LongOutput *= 25
		base.LongRead *= 25
		base.LongWrite = 0
		base.FastInput, base.FastOutput, base.FastRead, base.FastWrite = 0, 0, 0, 0
		return base
	}
	return p
}

// Published Standard-speed Codex credit rates per million tokens, verified
// 2026-09-23: https://learn.chatgpt.com/docs/pricing. Included Pro quota is
// account-wide and cannot be inferred from these credit prices alone.
func officialCodexCreditPrice(model string) (price, bool) {
	model = normalizeModel(model)
	var input, cached, output float64
	switch model {
	case "gpt-6-astra":
		input, cached, output = 250, 25, 1250
	case "gpt-6-sol":
		input, cached, output = 50, 5, 250
	case "gpt-6-luna":
		input, cached, output = 2.5, .25, 12.5
	case "gpt-5.6", "gpt-5.6-sol", "daybreak-blue", "gpt-daybreak-blue-latest":
		input, cached, output = 100, 10, 500
	case "daybreak-red", "gpt-5.6-cyber", "gpt-daybreak-red-latest":
		input, cached, output = 312.5, 31.25, 1875
	case "gpt-5.6-terra":
		input, cached, output = 50, 5, 300
	case "gpt-5.6-luna":
		input, cached, output = 5, .5, 30
	case "gpt-rosalind-research":
		input, cached, output = 125, 12.5, 625
	case "gpt-5.5":
		input, cached, output = 125, 12.5, 750
	case "gpt-5.4":
		input, cached, output = 62.5, 6.25, 375
	case "gpt-5.4-mini":
		input, cached, output = 18.75, 1.875, 113
	default:
		return price{}, false
	}
	return price{
		Model: model, Input: input, CacheRead: cached, Output: output,
		// Codex lists no separate cache-write or long-context credit charge.
		LongInput: input, LongRead: cached, LongOutput: output,
		Source: "https://learn.chatgpt.com/docs/pricing",
	}, true
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
		// Keep the verified launch rate card authoritative over catalog lag.
		if official, ok := officialGPT6Price(p.Model); ok {
			p = official
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models.dev catalog has no usable OpenAI prices")
	}
	return out, nil
}

// Verified OpenAI Standard rates for the actively used Codex models:
// https://developers.openai.com/api/docs/pricing
func officialGPT6Price(model string) (price, bool) {
	p := price{Model: normalizeModel(model), Source: "https://developers.openai.com/api/docs/pricing", UpdatedAt: time.Now().Unix()}
	switch p.Model {
	case "gpt-6-astra":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 10, 50, 1, 12.5
	case "gpt-6-sol":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 2, 10, .2, 2.5
	case "gpt-6-luna":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = .1, .5, .01, .125
	case "gpt-5.6", "gpt-5.6-sol":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 4, 20, .4, 5
	case "gpt-5.6-terra":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 2, 12, .2, 2.5
	case "gpt-5.6-luna":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = .2, 1.2, .02, .25
	default:
		return price{}, false
	}
	p.LongInput, p.LongOutput, p.LongRead, p.LongWrite = p.Input*2, p.Output*1.5, p.CacheRead*2, p.CacheWrite*2
	p.FastInput, p.FastOutput, p.FastRead, p.FastWrite = p.Input*2, p.Output*2, p.CacheRead*2, p.CacheWrite*2
	return p, true
}

func seedPrices(ctx context.Context, s *store) error {
	now := time.Now().Unix()
	sol, _ := officialGPT6Price("gpt-6-sol")
	luna, _ := officialGPT6Price("gpt-6-luna")
	return s.upsertPrices(ctx, []price{
		sol, luna,
		{Model: "gpt-6-astra", Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5, LongInput: 20, LongOutput: 75, LongRead: 2, LongWrite: 25, FastInput: 20, FastOutput: 100, FastRead: 2, FastWrite: 25, Source: "built-in fallback", UpdatedAt: now},
		{Model: "gpt-5.6-sol", Input: 4, Output: 20, CacheRead: .4, CacheWrite: 5, LongInput: 8, LongOutput: 30, LongRead: .8, LongWrite: 10, FastInput: 8, FastOutput: 40, FastRead: .8, FastWrite: 10, Source: "built-in fallback", UpdatedAt: now},
		{Model: "gpt-5.6-luna", Input: .2, Output: 1.2, CacheRead: .02, CacheWrite: .25, LongInput: .4, LongOutput: 1.8, LongRead: .04, LongWrite: .5, FastInput: .4, FastOutput: 2.4, FastRead: .04, FastWrite: .5, Source: "built-in fallback", UpdatedAt: now},
		{Model: "gpt-5.6-terra", Input: 2, Output: 12, CacheRead: .2, CacheWrite: 2.5, LongInput: 4, LongOutput: 18, LongRead: .4, LongWrite: 5, FastInput: 4, FastOutput: 24, FastRead: .4, FastWrite: 5, Source: "built-in fallback", UpdatedAt: now},
	})
}

func calculateCost(p price, d usageDetail, serviceTier string, cfg config) float64 {
	mode := normalizePricingMode(cfg.PricingMode)
	effective, _ := cfg.effectiveModelPrice(p)
	in, out, read, write := effective.Input, effective.Output, effective.CacheRead, effective.CacheWrite
	threshold := cfg.LongContextThreshold
	if mode == pricingModeCustom {
		threshold = cfg.CustomLongThreshold
	}
	if threshold <= 0 {
		threshold = 272000
	}
	if d.InputTokens > threshold && (mode != pricingModeCustom || cfg.CustomLongContext) {
		if learned, ok := cfg.learnedLongMultiplier(); ok {
			// The fit measures the absolute long/standard ratio. It replaces
			// the official long tier, just as a learned Fast ratio does.
			in *= learned.Value
			out *= learned.Value
			read *= learned.Value
			write *= learned.Value
		} else {
			if effective.LongInput > 0 {
				in = effective.LongInput
			}
			if effective.LongOutput > 0 {
				out = effective.LongOutput
			}
			if effective.LongRead > 0 {
				read = effective.LongRead
			}
			if effective.LongWrite > 0 {
				write = effective.LongWrite
			}
		}
	}
	if isFastTier(serviceTier) {
		fast := cfg.effectiveFastMultiplier(p)
		in *= fast
		out *= fast
		read *= fast
		write *= fast
	}
	if mode == pricingModeAPI {
		switch strings.ToLower(strings.TrimSpace(serviceTier)) {
		case "batch", "flex":
			in *= .5
			out *= .5
			read *= .5
			write *= .5
		}
	}
	cacheRead := max(d.CacheReadTokens, d.CachedTokens)
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
