package service

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

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
	if mode != OpenAIPromptCacheCreationOptimizationModeFree && mode != OpenAIPromptCacheCreationOptimizationModeInput125 {
		return body, false
	}
	// Streaming callers see many pre-token events. Avoid a JSON decode unless
	// the frame can actually carry a positive cache-creation bucket.
	if !openAIUsageBytesContainCacheCreationAlias(body) {
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
		changed = normalizeOpenAIUsageObjectForDownstream(usage, mode) || changed
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

func normalizeOpenAIWSDownstreamCacheUsage(message []byte, eventType string, mode string) []byte {
	if mode == "" || !shouldNormalizeOpenAIStreamUsageForDownstream(message, eventType) {
		return message
	}
	if normalized, changed := normalizeOpenAIDownstreamUsageJSON(message, mode); changed {
		return normalized
	}
	return message
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
	ordinary := input - cacheRead - creation
	if ordinary < 0 {
		ordinary = 0
	}
	if mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
		ordinary = saturatingAddInt64(ordinary, cacheCreationAsInput125(creation))
	}
	newInput := saturatingAddInt64(ordinary, cacheRead)
	usage[inputKey] = newInput
	if alternate := map[string]string{"input_tokens": "prompt_tokens", "prompt_tokens": "input_tokens"}[inputKey]; alternate != "" {
		if _, exists := usage[alternate]; exists {
			usage[alternate] = newInput
		}
	}
	zeroOpenAICacheCreationAliases(usage)
	if _, exists := usage["total_tokens"]; exists {
		output, _ := firstJSONIntWithKey(usage, "output_tokens", "completion_tokens")
		usage["total_tokens"] = saturatingAddInt64(newInput, output)
	}
	return true
}

func cacheCreationAsInput125(tokens int64) int64 {
	if tokens <= 0 {
		return 0
	}
	if tokens > (math.MaxInt64-3)/5 {
		return math.MaxInt64
	}
	return (tokens*5 + 3) / 4
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

func firstJSONIntWithKey(object map[string]any, keys ...string) (int64, string) {
	firstValidKey := ""
	for _, key := range keys {
		if value, ok := jsonInt64(object[key]); ok {
			if firstValidKey == "" {
				firstValidKey = key
			}
			if value != 0 {
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

func zeroOpenAICacheCreationAliases(usage map[string]any) {
	for _, key := range []string{"cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens"} {
		if _, exists := usage[key]; exists {
			usage[key] = int64(0)
		}
	}
	for _, detailsKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
		details, ok := usage[detailsKey].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"cache_creation_input_tokens", "cache_write_input_tokens", "cache_write_tokens", "cache_creation_tokens"} {
			if _, exists := details[key]; exists {
				details[key] = int64(0)
			}
		}
	}
}

func normalizeOpenAIResponsesUsageForDownstream(usage *apicompat.ResponsesUsage, mode string) bool {
	if usage == nil || usage.CacheCreationInputTokens <= 0 ||
		(mode != OpenAIPromptCacheCreationOptimizationModeFree && mode != OpenAIPromptCacheCreationOptimizationModeInput125) {
		return false
	}
	cacheRead := 0
	if usage.InputTokensDetails != nil {
		cacheRead = usage.InputTokensDetails.CachedTokens
	}
	creation := usage.CacheCreationInputTokens
	ordinary := usage.InputTokens - cacheRead - creation
	if ordinary < 0 {
		ordinary = 0
	}
	if mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
		ordinary = int(minInt64(saturatingAddInt64(int64(ordinary), cacheCreationAsInput125(int64(creation))), math.MaxInt))
	}
	newInput := minInt64(saturatingAddInt64(int64(ordinary), int64(cacheRead)), math.MaxInt)
	usage.InputTokens = int(newInput)
	usage.TotalTokens = int(minInt64(saturatingAddInt64(newInput, int64(usage.OutputTokens)), math.MaxInt))
	usage.CacheCreationInputTokens = 0
	if details := usage.InputTokensDetails; details != nil {
		details.CacheCreationInputTokens = 0
		details.CacheCreationTokens = 0
		details.CacheWriteInputTokens = 0
		details.CacheWriteTokens = 0
	}
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

func minInt64(value int64, limit int) int64 {
	if value > int64(limit) {
		return int64(limit)
	}
	return value
}
