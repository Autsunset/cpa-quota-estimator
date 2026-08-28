package main

import "context"

var allowanceModelOrder = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.3-codex",
}

func (s *store) remainingModelAllowances(ctx context.Context, remainingValue float64, cfg config) ([]modelAllowance, error) {
	prices, err := s.listPrices(ctx)
	if err != nil {
		return nil, err
	}
	byModel := make(map[string]price, len(prices))
	for _, p := range prices {
		byModel[normalizeModel(p.Model)] = p
	}
	out := make([]modelAllowance, 0, len(allowanceModelOrder))
	for _, model := range allowanceModelOrder {
		p, ok := byModel[model]
		if !ok {
			continue
		}
		p = priceForPricingMode(p, cfg.PricingMode)
		if p.Input <= 0 && p.Output <= 0 && p.CacheRead <= 0 {
			continue
		}
		item := modelAllowance{
			Model:         model,
			PricingMode:   normalizePricingMode(cfg.PricingMode),
			ValueUnit:     pricingValueUnit(cfg.PricingMode),
			InputRate:     p.Input,
			OutputRate:    p.Output,
			CacheReadRate: p.CacheRead,
		}
		if remainingValue > 0 {
			item.InputTokens = tokensForValue(remainingValue, p.Input)
			item.OutputTokens = tokensForValue(remainingValue, p.Output)
			item.CacheReadTokens = tokensForValue(remainingValue, p.CacheRead)
		}
		out = append(out, item)
	}
	return out, nil
}

func tokensForValue(value, ratePerMillion float64) float64 {
	if value <= 0 || ratePerMillion <= 0 {
		return 0
	}
	return value * 1_000_000 / ratePerMillion
}
