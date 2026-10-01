package main

import "sort"

type calibrationGuidance struct {
	Model              string  `json:"model,omitempty"`
	TokenType          string  `json:"token_type,omitempty"`
	Parameter          string  `json:"parameter"`
	Action             string  `json:"action"`
	Reason             string  `json:"reason"`
	Source             string  `json:"source"`
	Priority           int     `json:"priority"`
	Completed          bool    `json:"completed"`
	ConditionalFisher  float64 `json:"conditional_fisher"`
	MinFisher          float64 `json:"min_fisher"`
	WithinCycleShareSD float64 `json:"within_cycle_share_sd"`
	MinShareSD         float64 `json:"min_share_sd"`
}

func guidanceFromFit(fit weightFit) []calibrationGuidance {
	active := map[string]bool{}
	for _, model := range fit.ObservedModels {
		active[model] = true
	}
	result := make([]calibrationGuidance, 0, len(fit.ComponentDiagnostics)+len(fit.PooledModels)+2)
	appendDiagnostic := func(d identifiabilityDiagnostic, source string, complete bool, action string) {
		if complete {
			return
		}
		if d.Model != "" && !active[d.Model] {
			return
		}
		reason := "conditional_information_low"
		if d.WithinCycleShareSD < d.MinShareSD {
			reason = "insufficient_variation"
		}
		if d.Unlocked && !complete {
			reason = "posterior_not_supported"
		}
		if complete {
			reason = "identified"
			action = "complete"
		}
		priority := 2
		if complete {
			priority = 0
		}
		if !complete && d.ConditionalFisher > 0 {
			priority = 3
		}
		result = append(result, calibrationGuidance{Model: d.Model, TokenType: d.TokenType, Parameter: d.Name, Action: action, Reason: reason, Source: source, Priority: priority, Completed: complete, ConditionalFisher: d.ConditionalFisher, MinFisher: d.MinFisher, WithinCycleShareSD: d.WithinCycleShareSD, MinShareSD: d.MinShareSD})
	}
	for _, d := range fit.ComponentDiagnostics {
		estimate := priorWeightEstimate(1, .5)
		for _, row := range fit.Models {
			if row.Model == d.Model {
				field := d.TokenType
				if field == "cache" {
					field = "cache_read"
				}
				estimate = componentEstimate(row, field)
				break
			}
		}
		action := "cache_contrast"
		if d.TokenType == "cache" {
			action = "cache_contrast"
		}
		if d.TokenType == "output" {
			action = "output_contrast"
		}
		appendDiagnostic(d, estimate.Source, supportedWeight(estimate), action)
	}
	for _, pool := range fit.PooledModels {
		appendDiagnostic(pool.Diagnostic, pool.Estimate.Source, pool.Applied, "model_contrast")
		if len(result) == 0 || result[len(result)-1].Parameter != pool.Diagnostic.Name {
			continue
		}
		last := &result[len(result)-1]
		if !pool.Applied && supportedWeight(pool.Estimate) {
			last.Reason = "needs_validation"
		}
	}
	for _, d := range fit.ModifierDiagnostics {
		if d.Name == "fast_long" && fit.FastLong != nil {
			for _, action := range []string{"fast_contrast", "long_contrast"} {
				appendDiagnostic(d, fit.FastLong.Source, false, action)
				result[len(result)-1].Reason = "combined_modes_confounded"
			}
			continue
		}
		estimate, action := fit.Fast, "fast_contrast"
		if d.Name == "long" {
			estimate, action = fit.LongContext, "long_contrast"
		}
		appendDiagnostic(d, estimate.Source, supportedWeight(estimate), action)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Completed != result[j].Completed {
			return !result[i].Completed
		}
		return result[i].Priority > result[j].Priority
	})
	return result
}
