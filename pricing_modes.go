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
	Source     string  `json:"source"`
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
	AnchorAdjustment       modelPriceAdjustment            `json:"anchor_adjustment"`
	PooledEstimate         *pooledModelEstimate            `json:"pooled,omitempty"`
	Guidance               []calibrationGuidance           `json:"guidance,omitempty"`
	DifferencePercent      float64                         `json:"difference_percent"`
	UncertaintyPercent     float64                         `json:"uncertainty_percent"`
	OfficialFastMultiplier float64                         `json:"official_fast_multiplier"`
	FastMultiplier         float64                         `json:"fast_multiplier"`
	FastSource             string                          `json:"fast_source"`
	LongSource             string                          `json:"long_source"`
	FastUncertaintyPercent float64                         `json:"fast_uncertainty_percent"`
	FastLong               *weightEstimate                 `json:"fast_long,omitempty"`
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
	return modelPriceAdjustment{Factor: 1, Low: 1, High: 1, Source: "prior"}
}

func priorPriceAdjustment() modelPriceAdjustment {
	prior := priorWeightEstimate(1, .5)
	return modelPriceAdjustment{Factor: 1, Low: prior.Low, High: prior.High, Source: "prior"}
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
	result := priorPriceAdjustment()
	if normalizePricingMode(c.PricingMode) == pricingModeCustom {
		return result
	}
	model = normalizeModel(model)
	if model == weightReferenceModel && field == "input" {
		result = unchangedPriceAdjustment()
		result.Source = "anchor"
		return result
	}
	if c.LearnedFit == nil || !c.LearnedFit.Available || field == "cache_write" {
		return result
	}
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
		pooledApplied := false
		if estimate.Source == "pooled" {
			typ := field
			if typ == "cache_read" {
				typ = "cache"
			}
			for _, pool := range c.LearnedFit.PooledModels {
				if pool.Model == model && pool.Applied && pool.Composition[typ].Tokens > 0 {
					pooledApplied = true
				}
			}
		}
		if !supportedWeight(estimate) && !pooledApplied {
			return result
		}
		result.Factor = estimate.Value / denominator
		result.Low = estimate.Low / denominator
		result.High = estimate.High / denominator
		result.Calibrated = supportedWeight(estimate)
		result.Source = "independent"
		if pooledApplied {
			result.Source = "pooled"
		}
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
	if model == anchor && field == "input" {
		result.Baseline = true
		result.Source = "anchor"
		return result
	}
	mine := c.adjustmentForComponent(model, field)
	// One input gauge converts the complete shape, including prior terms.
	// The original published table is retained separately in Official.
	anchorAdj := c.adjustmentForComponent(anchor, "input")
	result.Factor = mine.Factor / anchorAdj.Factor
	result.Low = mine.Low / math.Max(anchorAdj.High, 1e-12)
	result.High = mine.High / math.Max(anchorAdj.Low, 1e-12)
	result.Source = mine.Source
	result.Calibrated = mine.Calibrated && (anchorAdj.Calibrated || anchorAdj.Source == "anchor")
	if mine.Calibrated && anchorAdj.Calibrated && c.LearnedFit != nil {
		typ := field
		if typ == "cache_read" {
			typ = "cache"
		}
		left, right := -1, -1
		for i, name := range c.LearnedFit.ParameterNames {
			if name == componentParameter(model, typ) {
				left = i
			}
			if name == componentParameter(anchor, "input") {
				right = i
			}
		}
		cov := c.LearnedFit.Covariance
		if left >= 0 && right >= 0 && left < len(cov) && right < len(cov) && left < len(cov[left]) && right < len(cov[right]) && right < len(cov[left]) {
			variance := math.Max(0, cov[left][left]+cov[right][right]-2*cov[left][right])
			width := 1.96 * math.Sqrt(variance)
			result.Low, result.High = result.Factor*math.Exp(-width), result.Factor*math.Exp(width)
		}
	}
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
	// Cache writes are not learned, but share the same unit conversion.
	write := components["cache_write"].Factor
	base.CacheWrite *= write
	base.LongWrite *= write
	base.FastWrite *= write
	return base, adjustment
}

func (c config) officialFastMultiplier(p price) float64 {
	if normalizePricingMode(c.PricingMode) == pricingModeCredits {
		return 2
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
	if c.LearnedFit != nil && c.LearnedFit.Available && appliedWeight(c.LearnedFit.Fast) {
		return quotaFastApplied(c.LearnedFit, p.Model)
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
		appliedWeight(c.LearnedFit.LongContext) {
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
	anchor := normalizeModel(c.AnchorModel)
	if anchor == "" {
		anchor = weightReferenceModel
	}
	row.AnchorAdjustment = c.adjustmentForComponent(anchor, "input")
	row.FastSource, row.LongSource = "official", "official"
	if mode != pricingModeCustom && c.LearnedFit != nil && c.LearnedFit.Available {
		if appliedWeight(c.LearnedFit.Fast) {
			estimate := c.LearnedFit.Fast
			row.FastSource = estimate.Source
			if row.FastSource == "" {
				row.FastSource = "independent"
			}
			row.FastUncertaintyPercent = math.Max(math.Abs(estimate.Value-estimate.Low), math.Abs(estimate.High-estimate.Value)) / estimate.Value * 100
		}
		if appliedWeight(c.LearnedFit.LongContext) {
			row.LongSource = c.LearnedFit.LongContext.Source
			if row.LongSource == "" {
				row.LongSource = "independent"
			}
		}
		if combined, ok := quotaFastLongApplied(c.LearnedFit, p.Model); ok {
			row.FastLong = &combined
		}
	}
	if mode != pricingModeCustom && c.LearnedFit != nil {
		for _, pooled := range c.LearnedFit.PooledModels {
			if pooled.Model == row.Model {
				copy := pooled
				row.PooledEstimate = &copy
			}
		}
		for _, guidance := range c.LearnedFit.Guidance {
			if guidance.Model == row.Model || guidance.Model == "" {
				row.Guidance = append(row.Guidance, guidance)
			}
		}
	}
	if official.Input > 0 && official.LongInput > 0 {
		row.OfficialLongMultiplier = official.LongInput / official.Input
	}
	if mode == pricingModeCustom {
		row.FastSource, row.LongSource = "custom", "custom"
		row.AnchorAdjustment = unchangedPriceAdjustment()
		row.AnchorAdjustment.Source = "custom"
		for field, adjustment := range row.ComponentAdjustments {
			adjustment.Source = "custom"
			row.ComponentAdjustments[field] = adjustment
		}
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
