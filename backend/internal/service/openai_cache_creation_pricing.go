package service

import (
	"context"
	"math"
	"math/big"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// Pricing is a request-local snapshot. Shared catalog/channel prices and the
// original usage used for billing are never modified by presentation policies.
type OpenAICacheCreationRates struct {
	Input    int64 `json:"input"`
	Read     int64 `json:"read"`
	Creation int64 `json:"creation"`
	Output   int64 `json:"output"`
}

type OpenAICacheCreationPricingInterval struct {
	MinTokens int64                             `json:"min_tokens"`
	MaxTokens *int64                            `json:"max_tokens,omitempty"`
	Pricing   *OpenAICacheCreationPricingPolicy `json:"pricing,omitempty"`
}

type OpenAICacheCreationPricingPolicy struct {
	Intervals                   []OpenAICacheCreationPricingInterval `json:"intervals,omitempty"`
	Rates                       OpenAICacheCreationRates             `json:"rates"`
	Priority                    OpenAICacheCreationRates             `json:"priority"`
	LongContextThreshold        int64                                `json:"long_context_threshold,omitempty"`
	LongContextInclusive        bool                                 `json:"long_context_inclusive,omitempty"`
	LongContextInputMultiplier  int64                                `json:"long_context_input_multiplier,omitempty"`
	LongContextOutputMultiplier int64                                `json:"long_context_output_multiplier,omitempty"`
}

func (s *OpenAIGatewayService) openAICacheCreationPricingForContext(ctx context.Context, account *Account, model string, upstreamModels ...string) *OpenAICacheCreationPricingPolicy {
	if s == nil || s.billingService == nil || account == nil || !isOpenAICacheCreationTextModel(model) {
		return nil
	}
	var group *Group
	if ctx != nil {
		group, _ = ctx.Value(ctxkey.Group).(*Group)
		if !IsGroupContextValid(group) {
			group = nil
		}
	}
	input := PricingInput{Model: model, Group: group}
	if group != nil {
		input.GroupID = &group.ID
	}
	resolver := NewModelPricingResolver(s.channelService, s.billingService)
	resolved := resolver.Resolve(ctx, input)
	if resolved == nil || resolved.Mode != BillingModeToken {
		return nil
	}
	// These catalog models are repriced by time after resolution during real
	// billing. A WS session snapshot cannot lock a future billing window, so
	// retain raw usage unless a group/channel or complete explicit price applies.
	if resolved.Source != PricingSourceGroup && resolved.Source != PricingSourceChannel &&
		(isDeepSeekModel(model) || isGemini36FlashPricingModel(model) || isGeminiArgonPricingModel(model)) {
		p := resolved.BasePricing
		if p == nil || !p.PricingOverrideInput || !p.PricingOverrideOutput || !p.PricingOverrideCacheRead {
			return nil
		}
	}
	upstreamModel := model
	if len(upstreamModels) > 0 && strings.TrimSpace(upstreamModels[0]) != "" {
		upstreamModel = upstreamModels[0]
	}
	longEnabled := account.IsOpenAILongContextBillingEnabled() && (group == nil || group.LongContextPricingEnabled) && !(account.IsOpenAIOAuthLike() && openai.IsGPT61SolModelSpelling(upstreamModel))
	policy := openAICacheCreationPricingSnapshot(resolved.BasePricing, longEnabled && len(resolved.Intervals) == 0)
	if len(resolved.Intervals) == 0 {
		return policy
	}
	if policy == nil {
		policy = &OpenAICacheCreationPricingPolicy{}
	}
	for i := range resolved.Intervals {
		iv := &resolved.Intervals[i]
		interval := OpenAICacheCreationPricingInterval{MinTokens: int64(iv.MinTokens), Pricing: openAICacheCreationPricingSnapshot(intervalToModelPricing(iv, resolved.BasePricing, resolved.SupportsCacheBreakdown, resolved.channelPricing), false)}
		if iv.MaxTokens != nil {
			upper := int64(*iv.MaxTokens)
			interval.MaxTokens = &upper
		}
		policy.Intervals = append(policy.Intervals, interval)
	}
	return policy
}

func openAICacheCreationPricingSnapshot(pricing *ModelPricing, longEnabled bool) *OpenAICacheCreationPricingPolicy {
	if pricing == nil || pricing.SupportsCacheBreakdown {
		return nil
	}
	p := *pricing
	for _, price := range []float64{p.InputPricePerToken, p.OutputPricePerToken, p.CacheReadPricePerToken, p.CacheCreationPricePerToken, p.InputPricePerTokenPriority, p.OutputPricePerTokenPriority, p.CacheReadPricePerTokenPriority, p.CacheCreationPricePerTokenPriority} {
		if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) || (price > 0 && openAIDownstreamCacheMarkupPriceUnits(price) <= 0) {
			return nil
		}
	}
	if p.CacheCreationPricePerToken <= 0 && !p.CacheCreationPriceExplicit {
		return nil
	}
	rates := OpenAICacheCreationRates{
		Input:    openAIDownstreamCacheMarkupPriceUnits(p.InputPricePerToken),
		Read:     openAIDownstreamCacheMarkupPriceUnits(p.CacheReadPricePerToken),
		Creation: openAIDownstreamCacheMarkupPriceUnits(p.CacheCreationPricePerToken),
		Output:   openAIDownstreamCacheMarkupPriceUnits(p.OutputPricePerToken),
	}
	if rates.Input <= 0 || rates.Output <= 0 {
		return nil
	}
	priority := rates
	if p.InputPricePerTokenPriority > 0 {
		priority.Input = openAIDownstreamCacheMarkupPriceUnits(p.InputPricePerTokenPriority)
	}
	if p.CacheReadPricePerTokenPriority > 0 {
		priority.Read = openAIDownstreamCacheMarkupPriceUnits(p.CacheReadPricePerTokenPriority)
	}
	if p.CacheCreationPricePerTokenPriority > 0 {
		priority.Creation = openAIDownstreamCacheMarkupPriceUnits(p.CacheCreationPricePerTokenPriority)
	}
	if p.OutputPricePerTokenPriority > 0 {
		priority.Output = openAIDownstreamCacheMarkupPriceUnits(p.OutputPricePerTokenPriority)
	}
	policy := &OpenAICacheCreationPricingPolicy{Rates: rates, Priority: priority}
	if longEnabled && p.LongContextInputThreshold > 0 && (p.LongContextInputMultiplier > 1 || p.LongContextOutputMultiplier > 1) {
		for _, multiplier := range []float64{p.LongContextInputMultiplier, p.LongContextOutputMultiplier} {
			if multiplier <= 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) || multiplier*1_000_000 >= float64(math.MaxInt64) {
				return nil
			}
		}
		policy.LongContextThreshold = int64(p.LongContextInputThreshold)
		policy.LongContextInclusive = p.LongContextThresholdInclusive
		policy.LongContextInputMultiplier = int64(math.Round(p.LongContextInputMultiplier * 1_000_000))
		policy.LongContextOutputMultiplier = int64(math.Round(p.LongContextOutputMultiplier * 1_000_000))
	}
	return policy
}

func (p OpenAICacheCreationPricingPolicy) longContext(input int64) bool {
	return p.LongContextThreshold > 0 && (input > p.LongContextThreshold || (p.LongContextInclusive && input == p.LongContextThreshold))
}

func (p *OpenAICacheCreationPricingPolicy) forInput(input int64) (*OpenAICacheCreationPricingPolicy, int) {
	if p == nil {
		return nil, -1
	}
	for i, iv := range p.Intervals {
		if input > iv.MinTokens && (iv.MaxTokens == nil || input <= *iv.MaxTokens) {
			return iv.Pricing, i
		}
	}
	return p, -1
}

func allocateOpenAICacheCreationPricedUsage(ordinary, read, creation, output int64, allowOutput bool, tier string, policy *OpenAICacheCreationPricingPolicy) (openAIDownstreamDisplayUsage, bool) {
	unchanged := openAIDownstreamDisplayUsage{ordinary: ordinary, cacheRead: read, creation: creation, output: output}
	if ordinary < 0 || read < 0 || creation <= 0 || output < 0 || ordinary > math.MaxInt64-read || ordinary+read > math.MaxInt64-creation {
		return unchanged, false
	}
	originalInput := saturatingAddInt64(saturatingAddInt64(ordinary, read), creation)
	rootPolicy := policy
	policy, intervalIndex := policy.forInput(originalInput)
	if policy == nil {
		return unchanged, false
	}
	rates := policy.Rates
	if normalizeBillingServiceTier(tier) == "priority" {
		rates = policy.Priority
	}
	if rates.Input <= 0 || rates.Output <= 0 || rates.Read < 0 || rates.Creation < 0 || ordinary < 0 || read < 0 || creation <= 0 || output < 0 {
		return unchanged, false
	}
	long := policy.longContext(originalInput)
	if long {
		rates.Input = multiplyOpenAIPriceUnits(rates.Input, policy.LongContextInputMultiplier)
		rates.Read = multiplyOpenAIPriceUnits(rates.Read, policy.LongContextInputMultiplier)
		rates.Creation = multiplyOpenAIPriceUnits(rates.Creation, policy.LongContextInputMultiplier)
		rates.Output = multiplyOpenAIPriceUnits(rates.Output, policy.LongContextOutputMultiplier)
		if rates.Input <= 0 || rates.Output <= 0 || rates.Read < 0 || rates.Creation < 0 {
			return unchanged, false
		}
	}
	// Keep the established GPT-5.6 allocation byte-for-byte when its effective
	// prices and long-context rules match the legacy twentieths calculation.
	legacyContext := (policy.LongContextThreshold == openAIGPT54LongContextInputThreshold && !policy.LongContextInclusive && policy.LongContextInputMultiplier == 2_000_000 && policy.LongContextOutputMultiplier == 1_500_000) ||
		(policy.LongContextThreshold == 0 && originalInput <= openAIGPT54LongContextInputThreshold)
	legacy := rates.Read > 0 && rates.Input%10 == 0 && rates.Read == rates.Input/10 && rates.Input%4 == 0 && rates.Creation%5 == 0 && rates.Creation/5 == rates.Input/4 &&
		((!long && rates.Output%6 == 0 && rates.Output/6 == rates.Input) || (long && rates.Input%2 == 0 && rates.Output%9 == 0 && rates.Output/9 == rates.Input/2)) &&
		legacyContext
	if legacy {
		if originalInput > math.MaxInt64/2 || output > math.MaxInt64-creation {
			return unchanged, false
		}
		allocation := allocateOpenAIInput125DisplayUsage(ordinary, read, creation, output, allowOutput)
		newInput := saturatingAddInt64(allocation.ordinary, allocation.cacheRead)
		_, newInterval := rootPolicy.forInput(newInput)
		if allocation.ordinary > math.MaxInt64-allocation.cacheRead || newInput > math.MaxInt64-allocation.output || newInterval != intervalIndex || policy.longContext(newInput) != long {
			return unchanged, false
		}
		return allocation, true
	}
	budget := new(big.Int).Mul(big.NewInt(creation), big.NewInt(rates.Creation))
	outputAdd := int64(0)
	if allowOutput {
		outputAdd = min(creation/100, output/10)
	}
	readAdd := int64(0)
	if rates.Read > 0 {
		readAdd = creation / 20
	}
	limitTokens := func(candidate, price int64) int64 {
		if price <= 0 {
			return 0
		}
		limit := new(big.Int).Quo(new(big.Int).Set(budget), big.NewInt(price))
		if limit.IsInt64() {
			candidate = min(candidate, limit.Int64())
		}
		budget.Sub(budget, new(big.Int).Mul(big.NewInt(candidate), big.NewInt(price)))
		return candidate
	}
	outputAdd = limitTokens(outputAdd, rates.Output)
	readAdd = limitTokens(readAdd, rates.Read)
	// Round the remaining amount to the nearest ordinary-input token.
	remaining := new(big.Int).Add(budget, big.NewInt(rates.Input/2))
	remaining.Quo(remaining, big.NewInt(rates.Input))
	if !remaining.IsInt64() {
		return unchanged, false
	}
	ordinaryAdd := remaining.Int64()
	if ordinary > math.MaxInt64-ordinaryAdd || read > math.MaxInt64-readAdd || output > math.MaxInt64-outputAdd {
		return unchanged, false
	}
	allocation := openAIDownstreamDisplayUsage{ordinary: ordinary + ordinaryAdd, cacheRead: read + readAdd, output: output + outputAdd}
	// Preserve the pricing tier. If conversion would change the context tier,
	// retain raw usage instead of repricing the entire original request.
	newInput := saturatingAddInt64(allocation.ordinary, allocation.cacheRead)
	_, newInterval := rootPolicy.forInput(newInput)
	if allocation.ordinary > math.MaxInt64-allocation.cacheRead || newInput > math.MaxInt64-allocation.output || newInterval != intervalIndex || policy.longContext(newInput) != long {
		return unchanged, false
	}
	return allocation, true
}

func multiplyOpenAIPriceUnits(price, multiplier int64) int64 {
	if multiplier <= 0 {
		return price
	}
	value := new(big.Int).Mul(big.NewInt(price), big.NewInt(multiplier))
	value.Add(value, big.NewInt(500_000)).Quo(value, big.NewInt(1_000_000))
	if !value.IsInt64() {
		return -1
	}
	return value.Int64()
}

func openAICacheCreationServiceTier(tier *string) string {
	if tier == nil || strings.TrimSpace(*tier) == "" {
		return "default"
	}
	return *tier
}

type openAICacheCreationServiceTierContextKey struct{}

// Carry the settlement request tier to response handlers. The original
// forwarding and billing paths remain unchanged even if the response differs.
func withOpenAICacheCreationServiceTier(ctx context.Context, account *Account, tier string) context.Context {
	if openAIDownstreamCacheUsageAccountMode(account) != OpenAIPromptCacheCreationOptimizationModeInput125 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(tier) == "" {
		tier = "default"
	}
	return context.WithValue(ctx, openAICacheCreationServiceTierContextKey{}, tier)
}

func (s *OpenAIGatewayService) openAICacheCreationPresentationModel(ctx context.Context, upstream, requested string) string {
	if s == nil || ctx == nil || strings.TrimSpace(requested) == "" {
		return upstream
	}
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	if !IsGroupContextValid(group) || s.channelService == nil {
		return upstream
	}
	mapping := s.channelService.ResolveChannelMapping(ctx, group.ID, requested)
	switch mapping.BillingModelSource {
	case BillingModelSourceRequested:
		return requested
	case BillingModelSourceChannelMapped:
		if mapping.MappedModel != "" && mapping.MappedModel != requested {
			return mapping.MappedModel
		}
	}
	return upstream
}
