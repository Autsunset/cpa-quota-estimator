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
	Model                  string               `json:"model"`
	Official               price                `json:"official"`
	OfficialListed         bool                 `json:"official_listed"`
	Calculated             price                `json:"calculated"`
	Adjustment             modelPriceAdjustment `json:"adjustment"`
	DifferencePercent      float64              `json:"difference_percent"`
	UncertaintyPercent     float64              `json:"uncertainty_percent"`
	OfficialFastMultiplier float64              `json:"official_fast_multiplier"`
	FastMultiplier         float64              `json:"fast_multiplier"`
	OfficialLongMultiplier float64              `json:"official_long_multiplier"`
	LongMultiplier         float64              `json:"long_multiplier"`
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

func (c config) adjustmentForModel(model string, baseInput float64) modelPriceAdjustment {
	result := modelPriceAdjustment{Factor: 1, Low: 1, High: 1}
	model = normalizeModel(model)
	if normalizePricingMode(c.PricingMode) == pricingModeCustom {
		return result
	}
	if c.LearnedFit == nil || !c.LearnedFit.Available || baseInput <= 0 {
		return result
	}
	mode := normalizePricingMode(c.PricingMode)
	refInput := c.baseInputForModel(weightReferenceModel, mode)
	if refInput <= 0 {
		refInput = 4
		if mode == pricingModeCredits {
			refInput = 100
		}
	}
	for _, row := range c.LearnedFit.Models {
		if row.Model != model {
			continue
		}
		if row.Input.PriorLocked || row.Input.Value <= 0 {
			return result
		}
		denominator := baseInput / refInput
		if denominator <= 0 {
			return result
		}
		result.Factor = row.Input.Value / denominator
		result.Low = row.Input.Low / denominator
		result.High = row.Input.High / denominator
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

func (c config) modelPriceAdjustment(p price) modelPriceAdjustment {
	mode := normalizePricingMode(c.PricingMode)
	result := modelPriceAdjustment{Factor: 1, Low: 1, High: 1}
	if mode == pricingModeCustom {
		return result
	}
	model := normalizeModel(p.Model)
	anchor := normalizeModel(c.AnchorModel)
	if anchor == "" {
		anchor = "gpt-5.6-sol"
	}
	if model == anchor {
		result.Baseline = true
		return result
	}
	base := priceForPricingMode(p, mode)
	mine := c.adjustmentForModel(model, base.Input)
	anchorAdj := c.adjustmentForModel(anchor, c.baseInputForModel(anchor, mode))
	if anchorAdj.Factor <= 0 {
		anchorAdj = modelPriceAdjustment{Factor: 1, Low: 1, High: 1}
	}
	result.Factor = mine.Factor / anchorAdj.Factor
	result.Low = mine.Low / math.Max(anchorAdj.High, 1e-12)
	result.High = mine.High / math.Max(anchorAdj.Low, 1e-12)
	result.Calibrated = mine.Calibrated
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
	factor := adjustment.Factor
	base.Input *= factor
	base.CacheRead *= factor
	base.Output *= factor
	base.CacheWrite *= factor
	base.LongInput *= factor
	base.LongRead *= factor
	base.LongOutput *= factor
	base.LongWrite *= factor
	base.FastInput *= factor
	base.FastRead *= factor
	base.FastOutput *= factor
	base.FastWrite *= factor
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
	if normalizePricingMode(c.PricingMode) == pricingModeCustom {
		return 1
	}
	if c.LearnedFit != nil && c.LearnedFit.Available && c.LearnedFit.LongContext.Value > 0 && !c.LearnedFit.LongContext.PriorLocked {
		return c.LearnedFit.LongContext.Value
	}
	return 1
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
	row := modelPriceRow{Model: normalizeModel(p.Model), Official: official, OfficialListed: listed, Calculated: calculated, Adjustment: adjustment,
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
	} else {
		row.LongMultiplier = row.OfficialLongMultiplier * c.effectiveLongMultiplier()
	}
	return row
}
