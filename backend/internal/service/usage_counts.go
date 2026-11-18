package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagetokens"
	"github.com/tidwall/gjson"
)

// sumUsageCounts reports invalid arithmetic separately; callers must not bill
// an overflow as a successful zero-token request.
func sumUsageCounts(values ...int) (int, bool) {
	n, ok := usagetokens.Add(values...)
	return n, !ok
}

func usageCount(value gjson.Result) (int, bool, bool) {
	if !value.Exists() || value.Type == gjson.Null {
		return 0, false, false
	}
	var raw any
	switch value.Type {
	case gjson.Number:
		raw = json.Number(value.Raw)
	case gjson.String:
		raw = value.Str
	default:
		return 0, false, true
	}
	n, ok := usagetokens.Parse(raw)
	return n, ok, !ok
}

// The first valid value, including zero, is authoritative within one envelope.
func usageAlias(value gjson.Result, paths ...string) (int, bool, bool) {
	invalid := false
	for _, path := range paths {
		n, present, bad := usageCount(value.Get(path))
		invalid = invalid || bad
		if present {
			return n, true, invalid
		}
	}
	return 0, false, invalid
}

// anthropicUsagePatchFromNode preserves raw TTL presence across partial events.
func anthropicUsagePatchFromNode(node gjson.Result, start bool) *sseUsagePatch {
	if !node.IsObject() {
		return nil
	}
	return buildAnthropicUsagePatch(start, func(paths ...string) (int, bool, bool) {
		return usageAlias(node, paths...)
	})
}

func anthropicUsagePatchFromMap(node map[string]any, start bool) *sseUsagePatch {
	return buildAnthropicUsagePatch(start, func(paths ...string) (int, bool, bool) {
		invalid := false
		for _, path := range paths {
			var value any = node
			for _, key := range strings.Split(path, ".") {
				object, ok := value.(map[string]any)
				if !ok {
					value = nil
					break
				}
				value = object[key]
			}
			if value == nil {
				continue
			}
			n, ok := usagetokens.Parse(value)
			invalid = invalid || !ok
			if ok {
				return n, true, invalid
			}
		}
		return 0, false, invalid
	})
}

func buildAnthropicUsagePatch(start bool, count func(...string) (int, bool, bool)) *sseUsagePatch {
	patch := &sseUsagePatch{}
	read := func(paths ...string) (int, bool) {
		n, present, invalid := count(paths...)
		patch.invalid = patch.invalid || invalid
		return n, present
	}
	patch.inputTokens, patch.hasInputTokens = read("input_tokens")
	patch.outputTokens, patch.hasOutputTokens = read("output_tokens")
	if !start {
		patch.hasInputTokens = patch.hasInputTokens && patch.inputTokens > 0
		patch.hasOutputTokens = patch.hasOutputTokens && patch.outputTokens > 0
	}
	patch.cacheCreationInputTokens, patch.hasCacheCreationInput = read("cache_creation_input_tokens")
	patch.cacheReadInputTokens, patch.hasCacheReadInput = read("cache_read_input_tokens", "cached_tokens", "prompt_tokens_details.cached_tokens", "prompt_cache_hit_tokens")
	patch.cacheCreation5mTokens, patch.hasCacheCreation5m = read("cache_creation.ephemeral_5m_input_tokens")
	patch.cacheCreation1hTokens, patch.hasCacheCreation1h = read("cache_creation.ephemeral_1h_input_tokens")
	patch.promptTokens, _ = read("prompt_tokens")
	if n, present := read("prompt_cache_miss_tokens"); present {
		patch.promptCacheMissTokens = &n
	}
	return patch
}

func validateBillingUsageTokens(tokens UsageTokens) error {
	if _, ok := usagetokens.Add(tokens.InputTokens, tokens.CacheReadTokens, tokens.CacheCreationTokens, tokens.OutputTokens); !ok {
		return fmt.Errorf("invalid usage: token count is negative or aggregate overflows")
	}
	for _, n := range []int{tokens.CacheCreation5mTokens, tokens.CacheCreation1hTokens, tokens.ImageInputTokens, tokens.ImageCacheReadTokens, tokens.ImageOutputTokens} {
		if n < 0 {
			return fmt.Errorf("invalid usage: negative token count")
		}
	}
	return nil
}
