package main

import (
	"fmt"
	"math"
)

type customModelPrice struct {
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	Output     float64 `json:"output"`
	CacheWrite float64 `json:"cache_write"`
}

type modelPriceAdjustment struct {
	Factor     float64 `json:"factor"`
	Low        float64 `json:"low"`
	High       float64 `json:"high"`
	Calibrated bool    `json:"calibrated"`
	Baseline   bool    `json:"baseline"`
}

type modelPriceRow struct {
	Model                  string                          `json:"model"`
	Official               price                           `json:"official"`
	OfficialListed         bool                            `json:"official_listed"`
	Calculated             price                           `json:"calculated"`
	Adjustment             modelPriceAdjustment            `json:"adjustment"`
	ComponentAdjustments   map[string]modelPriceAdjustment `json:"component_adjustments"`
	DifferencePercent      float64                         `json:"difference_percent"`
	UncertaintyPercent     float64                         `json:"uncertainty_percent"`
	OfficialFastMultiplier float64                         `json:"official_fast_multiplier"`
	FastMultiplier         float64                         `json:"fast_multiplier"`
	OfficialLongMultiplier float64                         `json:"official_long_multiplier"`
	LongMultiplier         float64                         `json:"long_multiplier"`
	LongUncertaintyPercent float64                         `json:"long_uncertainty_percent"`
}

func validateCustomModelPrice(p customModelPrice) error {
	for _, rate := range []float64{p.Input, p.CacheRead, p.Output, p.CacheWrite} {
		if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1_000_000 {
			return fmt.Errorf("custom prices must be finite and between 0 and 1000000")
		}
	}
	return nil
}

func (c config) baseInputForModel(model, mode string) float64 {
	model = normalizeModel(model)
	if p, ok := c.PriceCatalog[model]; ok {
		return priceForPricingMode(p, mode).Input
	}
	if p, ok := officialCodexCreditPrice(model); ok {
		if mode == pricingModeCredits {
			return p.Input
		}
		return p.Input / 25
	}
	return 0
}

func unchangedPriceAdjustment() modelPriceAdjustment {
	return modelPriceAdjustment{Factor: 1, Low: 1, High: 1}
}

func componentEstimate(row learnedModelWeights, field string) weightEstimate {
	switch field {
	case "cache_read":
		return row.Cache
	case "output":
		return row.Output
	default:
		return row.Input
	}
}

func componentPrice(p price, field string) float64 {
	switch field {
	case "cache_read":
		return p.CacheRead
	case "output":
		return p.Output
	default:
		return p.Input
	}
}

func (c config) adjustmentForComponent(model, field string) modelPriceAdjustment {
	result := unchangedPriceAdjustment()
	if normalizePricingMode(c.PricingMode) == pricingModeCustom || c.LearnedFit == nil || !c.LearnedFit.Available || field == "cache_write" {
		return result
	}
	model = normalizeModel(model)
	p, ok := referencePrice(model, pricingModeCredits, c.PriceCatalog)
	ref, found := referencePrice(weightReferenceModel, pricingModeCredits, c.PriceCatalog)
	if !ok || !found || ref.Input <= 0 {
		return result
	}
	denominator := componentPrice(p, field) / ref.Input
	if denominator <= 0 {
		return result
	}
	for _, row := range c.LearnedFit.Models {
		if row.Model != model {
			continue
		}
		estimate := componentEstimate(row, field)
		if estimate.PriorLocked || !estimate.Identified || estimate.Value <= 0 {
			return result
		}
		result.Factor = estimate.Value / denominator
		result.Low = estimate.Low / denominator
		result.High = estimate.High / denominator
		result.Calibrated = true
		if result.Low <= 0 {
			result.Low = result.Factor
		}
		if result.High <= 0 {
			result.High = result.Factor
		}
		return result
	}
	return result
}

func (c config) componentPriceAdjustment(p price, field string) modelPriceAdjustment {
	result := unchangedPriceAdjustment()
	if normalizePricingMode(c.PricingMode) == pricingModeCustom {
		return result
	}
	model := normalizeModel(p.Model)
	anchor := normalizeModel(c.AnchorModel)
	if anchor == "" {
		anchor = weightReferenceModel
	}
	if model == anchor {
		result.Baseline = true
		return result
	}
	mine := c.adjustmentForComponent(model, field)
	// An unidentified component retains its own official rate rather than
	// inheriting an input or anchor adjustment unsupported by its own data.
	if !mine.Calibrated {
		return result
	}
	anchorAdj := c.adjustmentForComponent(anchor, field)
	result.Factor = mine.Factor / anchorAdj.Factor
	result.Low = mine.Low / math.Max(anchorAdj.High, 1e-12)
	result.High = mine.High / math.Max(anchorAdj.Low, 1e-12)
	result.Calibrated = true
	return result
}

// Retain the old response's input adjustment for existing API consumers.
func (c config) modelPriceAdjustment(p price) modelPriceAdjustment {
	return c.componentPriceAdjustment(p, "input")
}

func (c config) componentPriceAdjustments(p price) map[string]modelPriceAdjustment {
	result := make(map[string]modelPriceAdjustment)
	for _, field := range []string{"input", "cache_read", "output", "cache_write"} {
		result[field] = c.componentPriceAdjustment(p, field)
	}
	return result
}

func customLongRate(custom, standard, long, defaultRatio float64) float64 {
	if standard > 0 && long > 0 {
		return custom * long / standard
	}
	return custom * defaultRatio
}

func (c config) effectiveModelPrice(p price) (price, modelPriceAdjustment) {
	mode := normalizePricingMode(c.PricingMode)
	base := priceForPricingMode(p, mode)
	adjustment := c.modelPriceAdjustment(p)
	if mode == pricingModeCustom {
		if custom, ok := c.CustomPrices[normalizeModel(p.Model)]; ok {
			base.Input, base.CacheRead, base.Output, base.CacheWrite = custom.Input, custom.CacheRead, custom.Output, custom.CacheWrite
			base.LongInput = customLongRate(custom.Input, p.Input, p.LongInput, 2)
			base.LongRead = customLongRate(custom.CacheRead, p.CacheRead, p.LongRead, 2)
			base.LongOutput = customLongRate(custom.Output, p.Output, p.LongOutput, 1.5)
			base.LongWrite = customLongRate(custom.CacheWrite, p.CacheWrite, p.LongWrite, 2)
		}
		return base, adjustment
	}
	components := c.componentPriceAdjustments(p)
	input, cache, output := components["input"].Factor, components["cache_read"].Factor, components["output"].Factor
	base.Input *= input
	base.LongInput *= input
	base.FastInput *= input
	base.CacheRead *= cache
	base.LongRead *= cache
	base.FastRead *= cache
	base.Output *= output
	base.LongOutput *= output
	base.FastOutput *= output
	// Cache writes have no separate learner parameter and retain their rate.
	return base, adjustment
}

func (c config) officialFastMultiplier(p price) float64 {
	if normalizePricingMode(c.PricingMode) == pricingModeCredits {
		if normalizeModel(p.Model) == "gpt-5.4" {
			return 2
		}
		return 2.5
	}
	if p.Input > 0 && p.FastInput > 0 {
		return p.FastInput / p.Input
	}
	return 2
}

func (c config) effectiveFastMultiplier(p price) float64 {
	if normalizePricingMode(c.PricingMode) == pricingModeCustom {
		if c.CustomFastMultiplier > 0 {
			return c.CustomFastMultiplier
		}
		return 2
	}
	if c.LearnedFit != nil && c.LearnedFit.Available && c.LearnedFit.Fast.Value > 0 && !c.LearnedFit.Fast.PriorLocked {
		return c.LearnedFit.Fast.Value
	}
	return c.officialFastMultiplier(p)
}

func (c config) effectiveLongMultiplier() float64 {
	if learned, ok := c.learnedLongMultiplier(); ok {
		return learned.Value
	}
	return 1
}

func (c config) learnedLongMultiplier() (weightEstimate, bool) {
	if normalizePricingMode(c.PricingMode) != pricingModeCustom && c.LearnedFit != nil && c.LearnedFit.Available &&
		c.LearnedFit.LongContext.Value > 0 && !c.LearnedFit.LongContext.PriorLocked {
		return c.LearnedFit.LongContext, true
	}
	return weightEstimate{}, false
}

func (c config) priceRow(p price) modelPriceRow {
	mode := normalizePricingMode(c.PricingMode)
	official := priceForPricingMode(p, mode)
	if mode == pricingModeCustom {
		official = p
	}
	calculated, adjustment := c.effectiveModelPrice(p)
	listed := true
	if mode == pricingModeCredits {
		_, listed = officialCodexCreditPrice(p.Model)
	}
	row := modelPriceRow{Model: normalizeModel(p.Model), Official: official, OfficialListed: listed, Calculated: calculated, Adjustment: adjustment, ComponentAdjustments: c.componentPriceAdjustments(p),
		DifferencePercent:      (adjustment.Factor - 1) * 100,
		UncertaintyPercent:     math.Max(math.Abs(adjustment.Factor-adjustment.Low), math.Abs(adjustment.High-adjustment.Factor)) / math.Max(adjustment.Factor, 1e-12) * 100,
		OfficialFastMultiplier: c.officialFastMultiplier(p), FastMultiplier: c.effectiveFastMultiplier(p),
		OfficialLongMultiplier: 1, LongMultiplier: c.effectiveLongMultiplier()}
	if official.Input > 0 && official.LongInput > 0 {
		row.OfficialLongMultiplier = official.LongInput / official.Input
	}
	if mode == pricingModeCustom {
		if official.Input > 0 {
			row.DifferencePercent = (calculated.Input/official.Input - 1) * 100
		}
		row.UncertaintyPercent = 0
		if c.CustomLongContext {
			row.LongMultiplier = row.OfficialLongMultiplier
		} else {
			row.LongMultiplier = 1
		}
	} else if learned, ok := c.learnedLongMultiplier(); ok {
		row.LongMultiplier = learned.Value
		row.LongUncertaintyPercent = math.Max(math.Abs(learned.Value-learned.Low), math.Abs(learned.High-learned.Value)) / learned.Value * 100
	} else {
		row.LongMultiplier = row.OfficialLongMultiplier
	}
	return row
}
