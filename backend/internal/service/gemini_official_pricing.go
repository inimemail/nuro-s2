package service

import (
	"context"
	"strings"
	"time"
)

// The Developer API standard rate card lists the launch offer through
// 2026-12-31. Apply a copy at the operation's pricing time, never mutate the
// shared catalog or configured group/channel prices.
// Source: https://ai.google.dev/gemini-api/docs/pricing#gemini-3.6-flash
var gemini36LaunchOfferEnd = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

type geminiBillingTime struct {
	at       time.Time
	eligible *bool
}

func gemini36PricingAt(at time.Time, eligible *bool) time.Time {
	if eligible != nil && !*eligible {
		return gemini36LaunchOfferEnd
	}
	return at
}

// Preserve custom stats rules and the channel's explicit use of user prices.
// Only default model costs share the settlement's time and provider identity.
func applyGemini36AccountStatsCost(ctx context.Context, log *UsageLog, channels *ChannelService, billing *BillingService, account *Account, groupID int64, model string, tokens UsageTokens, total float64, at time.Time, eligible *bool) {
	if !isGemini36FlashPricingModel(model) || log == nil || billing == nil || account == nil || account.Platform != PlatformGemini {
		return
	}
	if channels != nil {
		channel, err := channels.GetChannelForGroup(ctx, groupID)
		if err != nil {
			return
		}
		if channel != nil {
			if cost := tryCustomRules(channel, account.ID, groupID, PlatformGemini, model, tokens, 1); cost != nil {
				log.AccountStatsCost = cost
				return
			}
			if channel.ApplyPricingToAccountStats {
				log.AccountStatsCost = &total
				return
			}
		}
	}
	pricing, err := billing.GetModelPricing(model)
	if err != nil || pricing == nil {
		return
	}
	pricing = applyGemini36OfficialPricing(model, pricing, gemini36PricingAt(at, eligible))
	cost := billing.computeTokenBreakdown(pricing, tokens, 1, "", true).TotalCost
	log.AccountStatsCost = &cost
}

func isGemini36FlashPricingModel(model string) bool {
	model = normalizeGeminiThinkingTierAlias(lastSegment(strings.ToLower(strings.TrimSpace(model))))
	return model == "gemini-3.6-flash" || model == "gemini-3-6-flash" || model == "gemini-3.6-flash-preview"
}

func applyGemini36OfficialPricing(model string, pricing *ModelPricing, at time.Time) *ModelPricing {
	if pricing == nil || !isGemini36FlashPricingModel(model) {
		return pricing
	}
	if at.IsZero() {
		at = time.Now()
	}
	copy := *pricing
	input, output, read := 1.5e-6, 7.5e-6, .15e-6
	if at.Before(gemini36LaunchOfferEnd) {
		input, output, read = .75e-6, 3.75e-6, .075e-6
	}
	if !copy.PricingOverrideInput {
		copy.InputPricePerToken = input
	}
	if !copy.PricingOverrideOutput {
		copy.OutputPricePerToken = output
	}
	if !copy.PricingOverrideCacheRead {
		copy.CacheReadPricePerToken = read
	}
	return &copy
}
