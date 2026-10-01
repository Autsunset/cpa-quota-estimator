package main

// Paid API/Credits and included quota use different Fast defaults.
// Keep these separate even though their Standard token-price shapes overlap.
func quotaFastDefault(model string) float64 {
	if normalizeModel(model) == "gpt-5.4" {
		return 2
	}
	return 2.5
}

func quotaFastApplied(fit *weightFit, model string) float64 {
	if fit != nil && fit.Available && appliedWeight(fit.Fast) {
		return quotaFastDefault(model) * fit.Fast.Value / 2.5
	}
	return quotaFastDefault(model)
}

func quotaLongApplied(fit *weightFit) float64 {
	if fit != nil && fit.Available && appliedWeight(fit.LongContext) {
		return fit.LongContext.Value
	}
	return 1
}

func quotaFastLongApplied(fit *weightFit, model string) (weightEstimate, bool) {
	if fit == nil || !fit.Available || fit.FastLong == nil || !appliedWeight(*fit.FastLong) {
		return weightEstimate{}, false
	}
	result := *fit.FastLong
	ratio := quotaFastDefault(model) / 2.5
	result.Value, result.Low, result.High = result.Value*ratio, result.Low*ratio, result.High*ratio
	return result, true
}

func apiFastTokenMultiplier(p price, typ string) float64 {
	standard, fast := p.Input, p.FastInput
	if typ == "cache" {
		standard, fast = p.CacheRead, p.FastRead
	}
	if typ == "output" {
		standard, fast = p.Output, p.FastOutput
	}
	if standard > 0 && fast > 0 {
		return fast / standard
	}
	return 2
}
