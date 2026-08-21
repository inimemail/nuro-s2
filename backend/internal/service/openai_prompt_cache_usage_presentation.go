package service

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/big"
	"math/bits"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const openAIDownstreamCacheMarkupPriceScale = 1_000_000_000_000_000

// OpenAIDownstreamCacheMarkupPolicy is copied into Edge plans. Prices use
// fixed-point units so Go and Rust produce the same terminal usage values.
type OpenAIDownstreamCacheMarkupPolicy struct {
	ThresholdTokens             int64 `json:"threshold_tokens,omitempty"`
	PercentBPS                  int64 `json:"percent_bps,omitempty"`
	InputPriceUnits             int64 `json:"input_price_units,omitempty"`
	CacheReadPriceUnits         int64 `json:"cache_read_price_units,omitempty"`
	OutputPriceUnits            int64 `json:"output_price_units,omitempty"`
	LongContextThreshold        int64 `json:"long_context_threshold,omitempty"`
	LongContextInclusive        bool  `json:"long_context_inclusive,omitempty"`
	LongContextInputMultiplier  int64 `json:"long_context_input_multiplier,omitempty"`
	LongContextOutputMultiplier int64 `json:"long_context_output_multiplier,omitempty"`
}

func (p OpenAIDownstreamCacheMarkupPolicy) enabled() bool {
	return p.ThresholdTokens >= 0 && p.PercentBPS > 0 && p.InputPriceUnits > 0 &&
		p.CacheReadPriceUnits > 0 && p.OutputPriceUnits > 0
}

func openAIDownstreamCacheMarkupPriceUnits(price float64) int64 {
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
		return 0
	}
	scaled := math.Round(price * openAIDownstreamCacheMarkupPriceScale)
	if scaled <= 0 || scaled > math.MaxInt64 {
		return 0
	}
	return int64(scaled)
}

func (s *OpenAIGatewayService) openAIDownstreamCacheMarkupPolicyForContext(
	ctx context.Context,
	account *Account,
	model string,
) OpenAIDownstreamCacheMarkupPolicy {
	if OpenAIImageGenerationIntentFromContext(ctx) || account == nil ||
		!account.IsOpenAIDownstreamCacheMarkupEnabled() || !isOpenAIGPTTextModel(model) || s == nil || s.billingService == nil {
		return OpenAIDownstreamCacheMarkupPolicy{}
	}
	pricing, err := s.billingService.GetModelPricing(model)
	if err != nil || pricing == nil {
		return OpenAIDownstreamCacheMarkupPolicy{}
	}
	policy := OpenAIDownstreamCacheMarkupPolicy{
		ThresholdTokens:             account.OpenAIDownstreamCacheMarkupThresholdTokens(),
		PercentBPS:                  account.OpenAIDownstreamCacheMarkupPercentBPS(),
		InputPriceUnits:             openAIDownstreamCacheMarkupPriceUnits(pricing.InputPricePerToken),
		CacheReadPriceUnits:         openAIDownstreamCacheMarkupPriceUnits(pricing.CacheReadPricePerToken),
		OutputPriceUnits:            openAIDownstreamCacheMarkupPriceUnits(pricing.OutputPricePerToken),
		LongContextThreshold:        int64(pricing.LongContextInputThreshold),
		LongContextInclusive:        pricing.LongContextThresholdInclusive,
		LongContextInputMultiplier:  int64(math.Round(pricing.LongContextInputMultiplier * 1_000_000)),
		LongContextOutputMultiplier: int64(math.Round(pricing.LongContextOutputMultiplier * 1_000_000)),
	}
	if !policy.enabled() {
		return OpenAIDownstreamCacheMarkupPolicy{}
	}
	return policy
}

// openAIDownstreamCacheUsageMode is intentionally narrower than the request
// optimization mode. A/B never rewrite downstream usage, and a disabled
// account remains a byte-exact response no-op.
func openAIDownstreamCacheUsageAccountMode(account *Account) string {
	if account == nil || !account.IsOpenAI() || account.IsShadow() || account.IsImagePoolMode() ||
		(account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey) {
		return ""
	}
	if mode := account.openAIDownstreamCacheUsageModeOverride; mode == OpenAIPromptCacheCreationOptimizationModeFree || mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
		return mode
	}
	if !account.IsOpenAIPromptCacheCreationOptimizationEnabled() {
		return ""
	}
	switch account.OpenAIPromptCacheCreationOptimizationMode() {
	case OpenAIPromptCacheCreationOptimizationModeFree:
		return OpenAIPromptCacheCreationOptimizationModeFree
	case OpenAIPromptCacheCreationOptimizationModeInput125:
		return OpenAIPromptCacheCreationOptimizationModeInput125
	default:
		return ""
	}
}

func openAIDownstreamCacheUsageMode(account *Account, model string) string {
	if !isOpenAIGPT56Model(model) {
		return ""
	}
	return openAIDownstreamCacheUsageAccountMode(account)
}

func openAIDownstreamCacheUsageModeForContext(ctx context.Context, account *Account, model string) string {
	if OpenAIImageGenerationIntentFromContext(ctx) {
		return ""
	}
	return openAIDownstreamCacheUsageMode(account, model)
}

// normalizeOpenAIDownstreamUsageJSON rewrites only known wire-level usage
// envelopes. Callers must parse and retain the original usage before invoking
// it so local billing, audit, and cache scheduling continue to use upstream
// truth.
func normalizeOpenAIDownstreamUsageJSON(body []byte, mode string) ([]byte, bool) {
	return normalizeOpenAIDownstreamUsageJSONWithMarkup(body, mode, OpenAIDownstreamCacheMarkupPolicy{})
}

func normalizeOpenAIDownstreamUsageJSONWithMarkup(
	body []byte,
	mode string,
	markup OpenAIDownstreamCacheMarkupPolicy,
) ([]byte, bool) {
	cacheCreationEnabled := mode == OpenAIPromptCacheCreationOptimizationModeFree || mode == OpenAIPromptCacheCreationOptimizationModeInput125
	markupEnabled := markup.enabled()
	if !cacheCreationEnabled && !markupEnabled {
		return body, false
	}
	// Streaming callers see many pre-token events. Avoid a JSON decode unless
	// the frame can actually carry a relevant usage bucket.
	if (!cacheCreationEnabled || !openAIUsageBytesContainCacheCreationAlias(body)) &&
		(!markupEnabled || !openAIUsageBytesContainCacheReadAlias(body)) {
		return body, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return body, false
	}
	object, ok := root.(map[string]any)
	if !ok {
		return body, false
	}
	changed := false
	for _, usage := range openAIKnownUsageObjects(object) {
		rawCacheRead := openAIUsageCacheReadTokens(usage)
		rawInput, _ := firstJSONIntWithKey(usage, "input_tokens", "prompt_tokens")
		if cacheCreationEnabled {
			changed = normalizeOpenAIUsageObjectForDownstream(usage, mode) || changed
		}
		changed = applyOpenAIDownstreamCacheMarkupObject(usage, rawInput, rawCacheRead, markup) || changed
	}
	if !changed {
		return body, false
	}
	updated, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return updated, true
}

func openAIUsageBytesContainCacheCreationAlias(body []byte) bool {
	return bytes.Contains(body, []byte(`"cache_creation`)) || bytes.Contains(body, []byte(`"cache_write`))
}

func openAIUsageBytesContainCacheReadAlias(body []byte) bool {
	return bytes.Contains(body, []byte(`"cached_tokens"`)) ||
		bytes.Contains(body, []byte(`"cache_read_input_tokens"`)) ||
		bytes.Contains(body, []byte(`"cache_read_tokens"`))
}

func shouldNormalizeOpenAIStreamUsageForDownstream(body []byte, eventType string) bool {
	if !openAIUsageBytesContainCacheCreationAlias(body) {
		return false
	}
	eventType = strings.ToLower(strings.TrimSpace(eventType))
	if openAIResponsesTerminalEventType(eventType) != "" || eventType == "error" {
		return true
	}
	// Chat Completions usage chunks do not carry a Responses `type` field.
	return eventType == "" && bytes.Contains(body, []byte(`"usage"`))
}

func normalizeOpenAIDownstreamUsageForRequest(body []byte, ctx context.Context, account *Account, model string) ([]byte, bool) {
	return normalizeOpenAIDownstreamUsageJSON(body, openAIDownstreamCacheUsageModeForContext(ctx, account, model))
}

func (s *OpenAIGatewayService) normalizeOpenAIDownstreamUsageForRequest(
	body []byte,
	ctx context.Context,
	account *Account,
	model string,
) ([]byte, bool) {
	return normalizeOpenAIDownstreamUsageJSONWithMarkup(
		body,
		openAIDownstreamCacheUsageModeForContext(ctx, account, model),
		s.openAIDownstreamCacheMarkupPolicyForContext(ctx, account, model),
	)
}

func normalizeOpenAIWSDownstreamCacheUsage(message []byte, eventType string, mode string) []byte {
	if mode == "" || !shouldNormalizeOpenAIStreamUsageForDownstream(message, eventType) {
		return message
	}
	if normalized, changed := normalizeOpenAIDownstreamUsageJSON(message, mode); changed {
		return normalized
	}
	return message
}

func normalizeOpenAIWSDownstreamUsage(
	message []byte,
	eventType string,
	mode string,
	markup OpenAIDownstreamCacheMarkupPolicy,
) []byte {
	if !shouldNormalizeOpenAIStreamUsageForDownstreamWithMarkup(message, eventType, mode, markup) {
		return message
	}
	if normalized, changed := normalizeOpenAIDownstreamUsageJSONWithMarkup(message, mode, markup); changed {
		return normalized
	}
	return message
}

func shouldNormalizeOpenAIStreamUsageForDownstreamWithMarkup(
	body []byte,
	eventType string,
	mode string,
	markup OpenAIDownstreamCacheMarkupPolicy,
) bool {
	if shouldNormalizeOpenAIStreamUsageForDownstream(body, eventType) {
		return true
	}
	if !markup.enabled() || !openAIUsageBytesContainCacheReadAlias(body) {
		return false
	}
	eventType = strings.ToLower(strings.TrimSpace(eventType))
	return openAIResponsesTerminalEventType(eventType) != "" || eventType == "error" ||
		(eventType == "" && bytes.Contains(body, []byte(`"usage"`)))
}

func openAIKnownUsageObjects(root map[string]any) []map[string]any {
	objects := make([]map[string]any, 0, 2)
	appendUsage := func(parent map[string]any) {
		if usage, ok := parent["usage"].(map[string]any); ok {
			objects = append(objects, usage)
		}
	}
	appendUsage(root)
	if nested, ok := root["response"].(map[string]any); ok {
		appendUsage(nested)
	}
	return objects
}

func normalizeOpenAIUsageObjectForDownstream(usage map[string]any, mode string) bool {
	creation := firstPositiveJSONInt(usage,
		[]string{"input_tokens_details", "cache_creation_input_tokens"},
		[]string{"prompt_tokens_details", "cache_creation_input_tokens"},
		[]string{"input_tokens_details", "cache_write_input_tokens"},
		[]string{"prompt_tokens_details", "cache_write_input_tokens"},
		[]string{"input_tokens_details", "cache_write_tokens"},
		[]string{"prompt_tokens_details", "cache_write_tokens"},
		[]string{"input_tokens_details", "cache_creation_tokens"},
		[]string{"prompt_tokens_details", "cache_creation_tokens"},
		[]string{"cache_write_tokens"},
		[]string{"cache_creation_input_tokens"},
		[]string{"cache_write_input_tokens"},
		[]string{"cache_creation_tokens"},
	)
	if creation <= 0 {
		return false
	}
	input, inputKey := firstJSONIntWithKey(usage, "input_tokens", "prompt_tokens")
	if inputKey == "" {
		return false
	}
	cacheRead := firstPositiveJSONInt(usage,
		[]string{"input_tokens_details", "cached_tokens"},
		[]string{"prompt_tokens_details", "cached_tokens"},
		[]string{"cache_read_input_tokens"},
		[]string{"cache_read_tokens"},
		[]string{"cached_tokens"},
	)
	ordinary := subtractInt64FloorZero(input, cacheRead)
	ordinary = subtractInt64FloorZero(ordinary, creation)
	output, outputKey := firstJSONIntWithKey(usage, "output_tokens", "completion_tokens")
	if mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
		allocation := allocateOpenAIInput125DisplayUsage(ordinary, cacheRead, creation, output, outputKey != "")
		ordinary = allocation.ordinary
		cacheRead = allocation.cacheRead
		creation = allocation.creation
		if outputKey != "" {
			usage[outputKey] = allocation.output
			if alternate := map[string]string{"output_tokens": "completion_tokens", "completion_tokens": "output_tokens"}[outputKey]; alternate != "" {
				if _, exists := usage[alternate]; exists {
					usage[alternate] = allocation.output
				}
			}
		}
	}
	creation = 0
	newInput := saturatingAddInt64(ordinary, cacheRead)
	newInput = saturatingAddInt64(newInput, creation)
	usage[inputKey] = newInput
	if alternate := map[string]string{"input_tokens": "prompt_tokens", "prompt_tokens": "input_tokens"}[inputKey]; alternate != "" {
		if _, exists := usage[alternate]; exists {
			usage[alternate] = newInput
		}
	}
	setOpenAICacheCreationAliases(usage, creation)
	setOpenAICacheReadAliases(usage, cacheRead, mode == OpenAIPromptCacheCreationOptimizationModeInput125, inputKey)
	if _, exists := usage["total_tokens"]; exists {
		output, _ = firstJSONIntWithKey(usage, "output_tokens", "completion_tokens")
		usage["total_tokens"] = saturatingAddInt64(newInput, output)
	}
	return true
}

func openAIUsageCacheReadTokens(usage map[string]any) int64 {
	return firstPositiveJSONInt(usage,
		[]string{"input_tokens_details", "cached_tokens"},
		[]string{"prompt_tokens_details", "cached_tokens"},
		[]string{"cache_read_input_tokens"},
		[]string{"cache_read_tokens"},
		[]string{"cached_tokens"},
	)
}

func applyOpenAIDownstreamCacheMarkupObject(
	usage map[string]any,
	rawInput int64,
	rawCacheRead int64,
	policy OpenAIDownstreamCacheMarkupPolicy,
) bool {
	if !policy.enabled() || rawCacheRead <= 0 || rawCacheRead < policy.ThresholdTokens {
		return false
	}
	input, inputKey := firstJSONIntWithKey(usage, "input_tokens", "prompt_tokens")
	output, outputKey := firstJSONIntWithKey(usage, "output_tokens", "completion_tokens")
	if inputKey == "" || outputKey == "" {
		return false
	}
	inputAdd, cacheReadAdd, outputAdd := openAIDownstreamCacheMarkupAdds(rawInput, rawCacheRead, policy)
	if inputAdd == 0 && cacheReadAdd == 0 && outputAdd == 0 {
		return false
	}
	cacheRead := openAIUsageCacheReadTokens(usage)
	newInput := saturatingAddInt64(input, saturatingAddInt64(inputAdd, cacheReadAdd))
	newCacheRead := saturatingAddInt64(cacheRead, cacheReadAdd)
	newOutput := saturatingAddInt64(output, outputAdd)
	usage[inputKey] = newInput
	if alternate := map[string]string{"input_tokens": "prompt_tokens", "prompt_tokens": "input_tokens"}[inputKey]; alternate != "" {
		if _, exists := usage[alternate]; exists {
			usage[alternate] = newInput
		}
	}
	usage[outputKey] = newOutput
	if alternate := map[string]string{"output_tokens": "completion_tokens", "completion_tokens": "output_tokens"}[outputKey]; alternate != "" {
		if _, exists := usage[alternate]; exists {
			usage[alternate] = newOutput
		}
	}
	setOpenAICacheReadAliases(usage, newCacheRead, true, inputKey)
	if _, exists := usage["total_tokens"]; exists {
		usage["total_tokens"] = saturatingAddInt64(newInput, newOutput)
	}
	return true
}

func openAIDownstreamCacheMarkupAdds(
	rawInput int64,
	rawCacheRead int64,
	policy OpenAIDownstreamCacheMarkupPolicy,
) (inputAdd, cacheReadAdd, outputAdd int64) {
	inputPrice := policy.InputPriceUnits
	cacheReadPrice := policy.CacheReadPriceUnits
	outputPrice := policy.OutputPriceUnits
	longContext := policy.LongContextThreshold > 0 &&
		(rawInput > policy.LongContextThreshold || (policy.LongContextInclusive && rawInput == policy.LongContextThreshold))
	if longContext {
		inputPrice = multiplyFixedPrice(inputPrice, policy.LongContextInputMultiplier)
		cacheReadPrice = multiplyFixedPrice(cacheReadPrice, policy.LongContextInputMultiplier)
		outputPrice = multiplyFixedPrice(outputPrice, policy.LongContextOutputMultiplier)
	}
	return roundedMarkupTokens(rawCacheRead, cacheReadPrice, policy.PercentBPS, inputPrice),
		roundedMarkupTokens(rawCacheRead, cacheReadPrice, policy.PercentBPS, cacheReadPrice),
		roundedMarkupTokens(rawCacheRead, cacheReadPrice, policy.PercentBPS, outputPrice)
}

func multiplyFixedPrice(price, multiplierMillionths int64) int64 {
	if price <= 0 || multiplierMillionths <= 0 {
		return price
	}
	return roundedBigMulDiv([]int64{price, multiplierMillionths}, 1_000_000)
}

func roundedMarkupTokens(cacheRead, cacheReadPrice, percentBPS, bucketPrice int64) int64 {
	if cacheRead <= 0 || cacheReadPrice <= 0 || percentBPS <= 0 || bucketPrice <= 0 {
		return 0
	}
	denominator := saturatingAddInt64(0, bucketPrice)
	if denominator > math.MaxInt64/30_000 {
		return 0
	}
	denominator *= 30_000 // 10000 basis points and three equal cost shares.
	return roundedBigMulDiv([]int64{cacheRead, cacheReadPrice, percentBPS}, denominator)
}

func roundedBigMulDiv(factors []int64, denominator int64) int64 {
	if denominator <= 0 {
		return 0
	}
	numerator := big.NewInt(1)
	for _, factor := range factors {
		if factor <= 0 {
			return 0
		}
		numerator.Mul(numerator, big.NewInt(factor))
	}
	denom := big.NewInt(denominator)
	numerator.Add(numerator, new(big.Int).Rsh(new(big.Int).Set(denom), 1))
	numerator.Quo(numerator, denom)
	if !numerator.IsInt64() {
		return math.MaxInt64
	}
	return numerator.Int64()
}

type openAIDownstreamDisplayUsage struct {
	ordinary  int64
	cacheRead int64
	creation  int64
	output    int64
}

// allocateOpenAIInput125DisplayUsage hides cache creation and redistributes its
// GPT-5.6-equivalent cost across regular input, cache read, and a small output share.
func allocateOpenAIInput125DisplayUsage(ordinary, cacheRead, creation, output int64, allowOutput bool) openAIDownstreamDisplayUsage {
	allocation := openAIDownstreamDisplayUsage{
		ordinary:  maxInt64(ordinary, 0),
		cacheRead: maxInt64(cacheRead, 0),
		creation:  maxInt64(creation, 0),
		output:    maxInt64(output, 0),
	}
	if allocation.creation <= 0 {
		return allocation
	}
	outputAdd := int64(0)
	if allowOutput {
		outputAdd = allocation.creation / 100
		if outputLimit := allocation.output / 10; outputAdd > outputLimit {
			outputAdd = outputLimit
		}
	}
	originalInput := saturatingAddInt64(saturatingAddInt64(allocation.ordinary, allocation.cacheRead), allocation.creation)
	longContext := originalInput > openAIGPT54LongContextInputThreshold
	outputWeight := int64(120)
	if longContext {
		// Long-context pricing multiplies input/cache buckets by 2 and output
		// by 1.5. In regular-input twentieths, output therefore weighs 90.
		outputWeight = 90
	}
	readAdd, ordinaryAdd := openAIInput125DisplayAdds(allocation.creation, outputAdd, outputWeight)
	if allowOutput && !longContext {
		transformedInput := saturatingAddInt64(
			saturatingAddInt64(allocation.ordinary, allocation.cacheRead),
			saturatingAddInt64(ordinaryAdd, readAdd),
		)
		if transformedInput > openAIGPT54LongContextInputThreshold {
			excess := transformedInput - openAIGPT54LongContextInputThreshold
			outputAdd = saturatingAddInt64(outputAdd, ceilMulDivInt64(excess, 20, outputWeight))
			// Residue rounding can leave only a handful of input tokens above
			// the boundary. This loop is fixed and never request-size dependent.
			for range 16 {
				readAdd, ordinaryAdd = openAIInput125DisplayAdds(allocation.creation, outputAdd, outputWeight)
				transformedInput = saturatingAddInt64(
					saturatingAddInt64(allocation.ordinary, allocation.cacheRead),
					saturatingAddInt64(ordinaryAdd, readAdd),
				)
				if transformedInput <= openAIGPT54LongContextInputThreshold {
					break
				}
				outputAdd = saturatingAddInt64(outputAdd, 1)
			}
		}
	}
	allocation.ordinary = saturatingAddInt64(allocation.ordinary, ordinaryAdd)
	allocation.cacheRead = saturatingAddInt64(allocation.cacheRead, readAdd)
	allocation.creation = 0
	allocation.output = saturatingAddInt64(allocation.output, outputAdd)
	return allocation
}

func maxInt64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}

func openAIInput125DisplayAdds(creation, outputAdd, outputWeight int64) (int64, int64) {
	readAdd := nearestInt64WithMod10(
		creation/20,
		requiredCacheReadResidue(creation, outputAdd, outputWeight),
	)
	return readAdd, weightedOpenAIInput125OrdinaryAdd(creation, outputAdd, readAdd, outputWeight)
}

func requiredCacheReadResidue(creation, outputAdd, outputWeight int64) int64 {
	// Work modulo 20 before multiplication to avoid overflow.
	base := ((creation%20)*25 - (outputAdd%20)*(outputWeight%20)) % 20
	if base < 0 {
		base += 20
	}
	bestResidue, bestDistance := int64(0), int64(math.MaxInt64)
	for residue := int64(0); residue < 10; residue++ {
		remaining := (base - 2*residue) % 20
		if remaining < 0 {
			remaining += 20
		}
		distance := remaining
		if distance > 10 {
			distance = 20 - distance
		}
		if distance < bestDistance {
			bestResidue, bestDistance = residue, distance
		}
	}
	return bestResidue
}

func weightedOpenAIInput125OrdinaryAdd(creation, outputAdd, readAdd, outputWeight int64) int64 {
	if creation <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(creation), 25)
	outputHi, outputLo := bits.Mul64(uint64(maxInt64(outputAdd, 0)), uint64(maxInt64(outputWeight, 0)))
	lo, borrow := bits.Sub64(lo, outputLo, 0)
	hi, _ = bits.Sub64(hi, outputHi, borrow)
	readHi, readLo := bits.Mul64(uint64(maxInt64(readAdd, 0)), 2)
	lo, borrow = bits.Sub64(lo, readLo, 0)
	hi, _ = bits.Sub64(hi, readHi, borrow)
	lo, carry := bits.Add64(lo, 10, 0)
	hi, _ = bits.Add64(hi, 0, carry)
	if hi >= 20 {
		return math.MaxInt64
	}
	quotient, _ := bits.Div64(hi, lo, 20)
	if quotient > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(quotient)
}

func ceilMulDivInt64(value, multiplier, divisor int64) int64 {
	if value <= 0 || multiplier <= 0 || divisor <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(value), uint64(multiplier))
	lo, carry := bits.Add64(lo, uint64(divisor-1), 0)
	hi, _ = bits.Add64(hi, 0, carry)
	if hi >= uint64(divisor) {
		return math.MaxInt64
	}
	quotient, _ := bits.Div64(hi, lo, uint64(divisor))
	if quotient > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(quotient)
}

func nearestInt64WithMod10(value, residue int64) int64 {
	if value < 0 {
		value = 0
	}
	residue = ((residue % 10) + 10) % 10
	up := (residue - value%10 + 10) % 10
	down := up - 10
	if value+down >= 0 && -down <= up {
		return value + down
	}
	return value + up
}

func saturatingAddInt64(a, b int64) int64 {
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func subtractInt64FloorZero(value, subtrahend int64) int64 {
	if value <= 0 {
		return 0
	}
	if subtrahend <= 0 {
		return value
	}
	if subtrahend >= value {
		return 0
	}
	return value - subtrahend
}

func firstJSONIntWithKey(object map[string]any, keys ...string) (int64, string) {
	firstValidKey := ""
	for _, key := range keys {
		if value, ok := jsonInt64(object[key]); ok {
			if firstValidKey == "" {
				firstValidKey = key
			}
			if value > 0 {
				return value, key
			}
		}
	}
	return 0, firstValidKey
}

func firstPositiveJSONInt(object map[string]any, paths ...[]string) int64 {
	for _, path := range paths {
		var current any = object
		for _, key := range path {
			nested, ok := current.(map[string]any)
			if !ok {
				current = nil
				break
			}
			current = nested[key]
		}
		if value, ok := jsonInt64(current); ok && value > 0 {
			return value
		}
	}
	return 0
}

func jsonInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case float64:
		if typed < math.MinInt64 || typed > math.MaxInt64 || math.Trunc(typed) != typed {
			return 0, false
		}
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func setOpenAICacheCreationAliases(usage map[string]any, value int64) {
	for _, key := range []string{"cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens"} {
		if _, exists := usage[key]; exists {
			usage[key] = value
		}
	}
	for _, detailsKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
		details, ok := usage[detailsKey].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"cache_creation_input_tokens", "cache_write_input_tokens", "cache_write_tokens", "cache_creation_tokens"} {
			if _, exists := details[key]; exists {
				details[key] = value
			}
		}
	}
}

func setOpenAICacheReadAliases(usage map[string]any, value int64, ensureCanonical bool, inputKey string) {
	for _, key := range []string{"cache_read_input_tokens", "cache_read_tokens", "cached_tokens"} {
		if _, exists := usage[key]; exists {
			usage[key] = value
		}
	}
	for _, detailsKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
		details, ok := usage[detailsKey].(map[string]any)
		if !ok {
			continue
		}
		if _, exists := details["cached_tokens"]; exists {
			details["cached_tokens"] = value
		}
	}
	if !ensureCanonical {
		return
	}
	detailsKey := "input_tokens_details"
	if inputKey == "prompt_tokens" {
		detailsKey = "prompt_tokens_details"
	}
	if details, ok := usage[detailsKey].(map[string]any); ok {
		details["cached_tokens"] = value
		return
	}
	usage[detailsKey] = map[string]any{"cached_tokens": value}
}

func normalizeOpenAIResponsesUsageForDownstream(usage *apicompat.ResponsesUsage, mode string) bool {
	if usage == nil || usage.CacheCreationInputTokens <= 0 ||
		(mode != OpenAIPromptCacheCreationOptimizationModeFree && mode != OpenAIPromptCacheCreationOptimizationModeInput125) {
		return false
	}
	cacheRead := 0
	if usage.InputTokensDetails != nil {
		cacheRead = max(usage.InputTokensDetails.CachedTokens, 0)
	}
	creation := usage.CacheCreationInputTokens
	ordinary64 := subtractInt64FloorZero(int64(usage.InputTokens), int64(cacheRead))
	ordinary64 = subtractInt64FloorZero(ordinary64, int64(creation))
	ordinary := int(minInt64(ordinary64, math.MaxInt))
	if mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
		allocation := allocateOpenAIInput125DisplayUsage(int64(ordinary), int64(cacheRead), int64(creation), int64(usage.OutputTokens), true)
		ordinary = int(minInt64(allocation.ordinary, math.MaxInt))
		cacheRead = int(minInt64(allocation.cacheRead, math.MaxInt))
		creation = int(minInt64(allocation.creation, math.MaxInt))
		usage.OutputTokens = int(minInt64(allocation.output, math.MaxInt))
		if usage.InputTokensDetails == nil {
			usage.InputTokensDetails = &apicompat.ResponsesInputTokensDetails{}
		}
	}
	creation = 0
	newInput := minInt64(saturatingAddInt64(saturatingAddInt64(int64(ordinary), int64(cacheRead)), int64(creation)), math.MaxInt)
	usage.InputTokens = int(newInput)
	usage.TotalTokens = int(minInt64(saturatingAddInt64(newInput, int64(usage.OutputTokens)), math.MaxInt))
	usage.CacheCreationInputTokens = creation
	if details := usage.InputTokensDetails; details != nil {
		details.CachedTokens = cacheRead
		details.CacheCreationInputTokens = 0
		details.CacheCreationTokens = 0
		details.CacheWriteInputTokens = 0
		details.CacheWriteTokens = 0
	}
	return true
}

func normalizeOpenAIResponsesUsageForDownstreamWithMarkup(
	usage *apicompat.ResponsesUsage,
	mode string,
	markup OpenAIDownstreamCacheMarkupPolicy,
) bool {
	if usage == nil {
		return false
	}
	rawInput := int64(usage.InputTokens)
	rawCacheRead := int64(0)
	if usage.InputTokensDetails != nil {
		rawCacheRead = int64(max(usage.InputTokensDetails.CachedTokens, 0))
	}
	changed := normalizeOpenAIResponsesUsageForDownstream(usage, mode)
	if !markup.enabled() || rawCacheRead <= 0 || rawCacheRead < markup.ThresholdTokens {
		return changed
	}
	inputAdd, cacheReadAdd, outputAdd := openAIDownstreamCacheMarkupAdds(rawInput, rawCacheRead, markup)
	if inputAdd == 0 && cacheReadAdd == 0 && outputAdd == 0 {
		return changed
	}
	if usage.InputTokensDetails == nil {
		usage.InputTokensDetails = &apicompat.ResponsesInputTokensDetails{}
	}
	usage.InputTokens = int(minInt64(
		saturatingAddInt64(int64(usage.InputTokens), saturatingAddInt64(inputAdd, cacheReadAdd)),
		math.MaxInt,
	))
	usage.InputTokensDetails.CachedTokens = int(minInt64(
		saturatingAddInt64(int64(usage.InputTokensDetails.CachedTokens), cacheReadAdd),
		math.MaxInt,
	))
	usage.OutputTokens = int(minInt64(saturatingAddInt64(int64(usage.OutputTokens), outputAdd), math.MaxInt))
	usage.TotalTokens = int(minInt64(
		saturatingAddInt64(int64(usage.InputTokens), int64(usage.OutputTokens)),
		math.MaxInt,
	))
	return true
}

func cloneOpenAIResponsesUsage(usage *apicompat.ResponsesUsage) *apicompat.ResponsesUsage {
	if usage == nil {
		return nil
	}
	cloned := *usage
	if usage.InputTokensDetails != nil {
		details := *usage.InputTokensDetails
		cloned.InputTokensDetails = &details
	}
	if usage.OutputTokensDetails != nil {
		details := *usage.OutputTokensDetails
		cloned.OutputTokensDetails = &details
	}
	return &cloned
}

func normalizeOpenAIResponsesStreamEventForDownstream(event *apicompat.ResponsesStreamEvent, mode string) bool {
	if event == nil || (mode != OpenAIPromptCacheCreationOptimizationModeFree && mode != OpenAIPromptCacheCreationOptimizationModeInput125) {
		return false
	}
	changed := false
	if event.Usage != nil {
		usage := cloneOpenAIResponsesUsage(event.Usage)
		if normalizeOpenAIResponsesUsageForDownstream(usage, mode) {
			event.Usage = usage
			changed = true
		}
	}
	if event.Response != nil && event.Response.Usage != nil {
		response := *event.Response
		usage := cloneOpenAIResponsesUsage(event.Response.Usage)
		if normalizeOpenAIResponsesUsageForDownstream(usage, mode) {
			response.Usage = usage
			event.Response = &response
			changed = true
		}
	}
	return changed
}

func normalizeOpenAIResponsesStreamEventForDownstreamWithMarkup(
	event *apicompat.ResponsesStreamEvent,
	mode string,
	markup OpenAIDownstreamCacheMarkupPolicy,
) bool {
	if event == nil {
		return false
	}
	changed := false
	if event.Usage != nil {
		usage := cloneOpenAIResponsesUsage(event.Usage)
		if normalizeOpenAIResponsesUsageForDownstreamWithMarkup(usage, mode, markup) {
			event.Usage = usage
			changed = true
		}
	}
	if event.Response != nil && event.Response.Usage != nil {
		response := *event.Response
		usage := cloneOpenAIResponsesUsage(event.Response.Usage)
		if normalizeOpenAIResponsesUsageForDownstreamWithMarkup(usage, mode, markup) {
			response.Usage = usage
			event.Response = &response
			changed = true
		}
	}
	return changed
}

func minInt64(value int64, limit int) int64 {
	if value > int64(limit) {
		return int64(limit)
	}
	return value
}
