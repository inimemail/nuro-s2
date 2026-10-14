package service

import (
	"context"
	"errors"
	"github.com/shopspring/decimal"
	"math"
	"strings"
	"time"
)

func (s *BillingService) GeminiCacheStoragePrice(model string, at time.Time) (float64, string, error) {
	return s.geminiCacheStoragePriceForAccount(model, at, nil)
}

func (s *BillingService) geminiCacheStoragePriceForAccount(model string, at time.Time, account *Account) (float64, string, error) {
	if s.pricingService != nil {
		if p := s.pricingService.GetIdentifiedModelPricing(model); p != nil && p.PricingOverrideCacheStorage {
			if p.CacheStorageCostPerTokenPerHour < 0 || math.IsNaN(p.CacheStorageCostPerTokenPerHour) || math.IsInf(p.CacheStorageCostPerTokenPerHour, 0) {
				return 0, "", errors.New("invalid explicit cache storage override")
			}
			return p.CacheStorageCostPerTokenPerHour, "override_file", nil
		}
	}
	if account != nil && !account.IsVertexServiceAccount() && account.IsThirdPartyGeminiAPIKey() {
		return 0, "", errors.New("third-party explicit cache requires a configured storage price")
	}
	name := lastSegment(strings.ToLower(strings.TrimSpace(model)))
	var rate float64
	switch {
	case isGemini36FlashPricingModel(name):
		rate = 1e-6
		if (account == nil || !account.IsVertexServiceAccount()) && at.Before(gemini36LaunchOfferEnd) {
			rate = .5e-6
		}
	case name == "gemini-2.5-flash" || name == "gemini-2.5-flash-lite" || name == "gemini-3-flash-preview" || name == "gemini-3.1-flash-lite-preview":
		rate = 1e-6
	case name == "gemini-2.5-pro" || name == "gemini-3-pro-preview" || name == "gemini-3.1-pro-preview" || name == "gemini-3.1-pro":
		rate = 4.5e-6
	default:
		return 0, "", errors.New("explicit cache storage pricing is unavailable for this model")
	}
	source := "official_gemini"
	if account != nil && account.IsVertexServiceAccount() {
		source = "official_vertex"
	}
	return rate, source, nil
}

func (s *GeminiCachedContentService) pricingContext(ctx context.Context, key *APIKey, account *Account, model, upstream string, tokens int64, at time.Time) (GeminiCachePricingContext, error) {
	resolved := s.resolver.Resolve(ctx, PricingInput{Model: model, GroupID: key.GroupID, Group: key.Group})
	if resolved == nil || resolved.Mode != BillingModeToken {
		return GeminiCachePricingContext{}, errors.New("cache management requires token pricing")
	}
	pricing := s.resolver.GetIntervalPricing(resolved, int(tokens))
	if pricing == nil {
		return GeminiCachePricingContext{}, ErrModelPricingUnavailable
	}
	if resolved.Source != PricingSourceGroup && resolved.Source != PricingSourceChannel {
		pricingAt := at
		if account.IsVertexServiceAccount() || account.IsThirdPartyGeminiAPIKey() {
			pricingAt = gemini36LaunchOfferEnd // Developer API promotion does not imply Vertex eligibility.
		}
		pricing = applyGemini36OfficialPricing(model, pricing, pricingAt)
	}
	storage, source, err := s.billing.geminiCacheStoragePriceForAccount(model, at, account)
	if resolved.channelPricing != nil && resolved.channelPricing.CacheStoragePrice != nil {
		storage, source, err = *resolved.channelPricing.CacheStoragePrice, resolved.Source, nil
		if storage < 0 || math.IsNaN(storage) || math.IsInf(storage, 0) {
			err = errors.New("invalid explicit cache storage price")
		}
	}
	if err != nil {
		return GeminiCachePricingContext{}, err
	}
	multiplier := s.gateway.ResolveUserGroupRateMultiplier(ctx, key.UserID, key.Group.ID, key.Group.RateMultiplier) * key.Group.PeakMultiplierAt(at) * resolvedChannelTimeMultiplier(resolved, at)
	costMultiplier := account.BillingRateMultiplier()
	if multiplier < 0 || costMultiplier < 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || math.IsNaN(costMultiplier) || math.IsInf(costMultiplier, 0) {
		return GeminiCachePricingContext{}, errors.New("invalid cache pricing multiplier")
	}
	costInput, costStorage, costSource, err := s.accountCachePricing(ctx, key, account, upstream, tokens, at, pricing.InputPricePerToken, storage, resolvedChannelTimeMultiplier(resolved, at))
	if err != nil {
		return GeminiCachePricingContext{}, err
	}
	result := GeminiCachePricingContext{Version: 2, AccountInputPerToken: decimal.NewFromFloat(costInput).String(), AccountStoragePerTokenHour: decimal.NewFromFloat(costStorage).String(), AccountSource: costSource, PricingAt: at.UTC(), Source: resolved.Source + "/" + source, InputPerToken: decimal.NewFromFloat(pricing.InputPricePerToken).String(), StoragePerTokenHour: decimal.NewFromFloat(storage).String(), UserMultiplier: decimal.NewFromFloat(multiplier).String(), AccountMultiplier: decimal.NewFromFloat(costMultiplier).String()}
	if resolved.channelPricing != nil && resolved.channelPricing.ChannelID > 0 {
		id := resolved.channelPricing.ChannelID
		result.ChannelID = &id
	}
	return result, nil
}

// Account costs follow the existing stats rule priority and the final upstream
// model, independently of the user's group rate and peak multiplier.
func (s *GeminiCachedContentService) accountCachePricing(ctx context.Context, key *APIKey, account *Account, upstream string, tokens int64, at time.Time, userInput, userStorage, channelTime float64) (float64, float64, string, error) {
	var custom *ChannelModelPricing
	if channels := s.resolver.channelService; channels != nil {
		channel, err := channels.GetChannelForGroup(ctx, key.Group.ID)
		if err != nil {
			return 0, 0, "", err
		}
		if channel != nil {
			for _, rule := range channel.AccountStatsPricingRules {
				if !matchAccountStatsRule(&rule, account.ID, key.Group.ID) {
					continue
				}
				custom = findPricingForModel(rule.Pricing, PlatformGemini, strings.ToLower(upstream))
				if custom != nil {
					break
				}
			}
			if custom == nil && channel.ApplyPricingToAccountStats {
				return userInput * channelTime, userStorage * channelTime, "channel_user_price", nil
			}
		}
	}
	pricing, err := s.billing.GetModelPricingWithChannel(upstream, custom)
	if err != nil || pricing == nil {
		return 0, 0, "", ErrModelPricingUnavailable
	}
	pricingAt := at
	if account.IsVertexServiceAccount() || account.IsThirdPartyGeminiAPIKey() {
		pricingAt = gemini36LaunchOfferEnd
	}
	if custom == nil {
		pricing = applyGemini36OfficialPricing(upstream, pricing, pricingAt)
	}
	storage, source, err := s.billing.geminiCacheStoragePriceForAccount(upstream, at, account)
	if custom != nil && custom.CacheStoragePrice != nil {
		storage, source, err = *custom.CacheStoragePrice, "account_stats_rule", nil
	}
	if err != nil {
		return 0, 0, "", err
	}
	if custom != nil {
		resolved := s.resolver.resolveConfiguredPricing(custom, upstream, PricingSourceChannel)
		pricing = s.resolver.GetIntervalPricing(resolved, int(tokens))
		if pricing == nil || resolved.Mode != BillingModeToken {
			return 0, 0, "", ErrModelPricingUnavailable
		}
		m := resolvedChannelTimeMultiplier(resolved, at)
		return pricing.InputPricePerToken * m, storage * m, source, nil
	}
	return pricing.InputPricePerToken, storage, source, nil
}
