package service

import (
	"strings"
	"time"
)

func gemini4BaseModel(model string) string {
	name := lastSegment(strings.ToLower(strings.TrimSpace(model)))
	for _, base := range []string{"gemini-4-carbon", "gemini-4-hydrogen"} {
		switch name {
		case base, base + "-low", base + "-medium", base + "-high", base + "-tiered":
			return base
		}
	}
	return ""
}

// Google publishes a calendar date without a timezone. The application uses
// UTC for this catalog boundary; explicit per-field overrides remain authoritative.
var gemini4CarbonStandardPriceStart = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

func applyGemini4OfficialPricing(model string, pricing *ModelPricing, at time.Time) *ModelPricing {
	base := gemini4BaseModel(model)
	if pricing == nil || base == "" {
		return pricing
	}
	if at.IsZero() {
		at = time.Now()
	}
	input, output, read := 1.5e-6, 7.5e-6, .075e-6
	if base == "gemini-4-carbon" {
		input, output, read = 2e-6, 10e-6, .1e-6
		if !at.Before(gemini4CarbonStandardPriceStart) {
			input, output, read = 4e-6, 20e-6, .2e-6
		}
	}
	cloned := *pricing
	if !cloned.PricingOverrideInput {
		cloned.InputPricePerToken = input
		if !cloned.vertexPriceOverrides["input_cost_per_token_priority"] {
			cloned.InputPricePerTokenPriority = input * 1.8
		}
	}
	if !cloned.PricingOverrideOutput {
		cloned.OutputPricePerToken = output
		if !cloned.vertexPriceOverrides["output_cost_per_token_priority"] {
			cloned.OutputPricePerTokenPriority = output * 1.8
		}
	}
	if !cloned.PricingOverrideCacheRead {
		cloned.CacheReadPricePerToken = read
		if !cloned.vertexPriceOverrides["cache_read_input_token_cost_priority"] {
			cloned.CacheReadPricePerTokenPriority = read * 1.8
		}
	}
	return &cloned
}

func qwen3VectorFallback(model string) *ModelPricing {
	name := lastSegment(strings.ToLower(strings.TrimSpace(model)))
	for _, family := range []string{"qwen3-embedding-", "qwen3-reranker-"} {
		var price float64
		switch name {
		case family + "8b":
			price = .04e-6
		case family + "4b":
			price = .02e-6
		case family + "0.6b":
			price = .01e-6
		default:
			continue
		}
		return &ModelPricing{InputPricePerToken: price, OutputPricePerToken: 0}
	}
	return nil
}
