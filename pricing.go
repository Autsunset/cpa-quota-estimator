package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		if official, ok := p.codexCreditPrice(); ok {
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

// Fallback Standard-speed Codex credit rates per million tokens from
// https://learn.chatgpt.com/docs/pricing. Included Pro quota is
// account-wide and cannot be inferred from these credit prices alone.
func officialCodexCreditPrice(model string) (price, bool) {
	model = normalizeModel(model)
	var input, cached, output float64
	switch model {
	case "gpt-6.2-astra":
		input, cached, output = 200, 10, 1000
	case "gpt-6.2-sol":
		input, cached, output = 50, 2.5, 250
	case "gpt-6-astra":
		input, cached, output = 250, 25, 1250
	case "gpt-6-sol":
		input, cached, output = 50, 5, 250
	case "gpt-6.1-sol":
		input, cached, output = 50, 2.5, 250
	case "gpt-6.1-terra":
		input, cached, output = 25, 1.25, 125
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
		Source: codexPricingURL,
	}, true
}

type catalogProvider struct {
	Models map[string]json.RawMessage `json:"models"`
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
				Input      *float64 `json:"input"`
				Output     *float64 `json:"output"`
				CacheRead  float64  `json:"cache_read"`
				CacheWrite float64  `json:"cache_write"`
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
		if entry.Cost.Input == nil || entry.Cost.Output == nil ||
			!validPriceRates(*entry.Cost.Input, *entry.Cost.Output, entry.Cost.CacheRead, entry.Cost.CacheWrite) ||
			(*entry.Cost.Input == 0 && *entry.Cost.Output == 0) {
			continue
		}
		p := price{Model: normalizeModel(model), Input: *entry.Cost.Input, Output: *entry.Cost.Output, CacheRead: entry.Cost.CacheRead, CacheWrite: entry.Cost.CacheWrite, Source: "models.dev/openai", UpdatedAt: now}
		if p.Model == "" {
			continue
		}
		for _, tier := range entry.Cost.Tiers {
			if strings.EqualFold(tier.Tier.Type, "context") &&
				validPriceRates(tier.Input, tier.Output, tier.CacheRead, tier.CacheWrite) {
				p.LongInput = tier.Input
				p.LongOutput = tier.Output
				p.LongRead = tier.CacheRead
				p.LongWrite = tier.CacheWrite
				break
			}
		}
		if fast, ok := entry.Experimental.Modes["fast"]; ok &&
			validPriceRates(fast.Cost.Input, fast.Cost.Output, fast.Cost.CacheRead, fast.Cost.CacheWrite) {
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

// Verified OpenAI Standard rates for the actively used Codex models:
// https://developers.openai.com/api/docs/pricing
func officialGPT6Price(model string) (price, bool) {
	p := price{Model: normalizeModel(model), Source: "https://developers.openai.com/api/docs/pricing", UpdatedAt: time.Now().Unix()}
	switch p.Model {
	case "gpt-6.2-astra":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 8, 40, .4, 10
	case "gpt-6.2-sol":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 2, 10, .1, 2.5
	case "gpt-6-astra":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 10, 50, 1, 12.5
	case "gpt-6-sol":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 2, 10, .2, 2.5
	case "gpt-6.1-sol":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 2, 10, .1, 2.5
	case "gpt-6.1-terra":
		p.Input, p.Output, p.CacheRead, p.CacheWrite = 1, 5, .05, 1.25
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
	prices := []price{{Model: "gpt-6-chat-thinking"}}
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol", "gpt-6-astra", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		p, _ := officialGPT6Price(model)
		prices = append(prices, p)
	}
	return s.writePriceCatalog(ctx, prices, nil, true, true)
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
	if mode != pricingModeCustom && d.InputTokens > threshold && isFastTier(serviceTier) {
		if combined, ok := quotaFastLongApplied(cfg.LearnedFit, p.Model); ok {
			cacheRead := max(d.CacheReadTokens, d.CachedTokens)
			uncached := max(int64(0), d.InputTokens-cacheRead-d.CacheCreationTokens)
			return (float64(uncached)*in + float64(cacheRead)*read + float64(d.CacheCreationTokens)*write + float64(d.OutputTokens)*out) / 1_000_000 * combined.Value
		}
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
