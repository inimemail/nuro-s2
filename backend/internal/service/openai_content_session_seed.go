package service

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// contentSessionSeedPrefix prevents collisions between content-derived seeds
// and explicit session IDs (e.g. "sess-xxx" or "compat_cc_xxx").
const contentSessionSeedPrefix = "compat_cs_"

const (
	openAIPromptCacheBoostStaticSeedPrefix                = "pcache_static_"
	openAIPromptCacheBoostAffinitySessionPrefix           = "pcache-affinity:"
	openAIPromptCacheBoostAggressiveAffinitySessionPrefix = "pcache-affinity-aggressive:"
	openAIPromptCacheBoostUpstreamAffinitySessionPrefix   = "pcache-affinity-upstream:"
	openAIPromptCacheBoostOptimizedAffinitySessionPrefix  = "pcache-affinity-upstream-v3:"
	openAIPromptCacheBoostMinStaticPrefixBytes            = 1024
	openAIPromptCacheBoostMediumStaticPrefixBytes         = 8192
	openAIPromptCacheBoostLargeStaticPrefixBytes          = 32768
	openAIPromptCacheBoostHugeStaticPrefixBytes           = 65536
	openAIPromptCacheBoostMaxLaneCount                    = 16
)

// deriveOpenAIContentSessionSeed builds a stable session seed from an
// OpenAI-format request body. Only fields constant across conversation turns
// are included: model, tools/functions definitions, system/developer prompts,
// instructions (Responses API), and the first user message.
// Supports both Chat Completions (messages) and Responses API (input).
func deriveOpenAIContentSessionSeed(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var b strings.Builder

	if model := gjson.GetBytes(body, "model").String(); model != "" {
		_, _ = b.WriteString("model=")
		_, _ = b.WriteString(model)
	}

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() && tools.Raw != "[]" {
		_, _ = b.WriteString("|tools=")
		_, _ = b.WriteString(normalizeCompatSeedJSON(json.RawMessage(tools.Raw)))
	}

	if funcs := gjson.GetBytes(body, "functions"); funcs.Exists() && funcs.IsArray() && funcs.Raw != "[]" {
		_, _ = b.WriteString("|functions=")
		_, _ = b.WriteString(normalizeCompatSeedJSON(json.RawMessage(funcs.Raw)))
	}

	if instr := gjson.GetBytes(body, "instructions").String(); instr != "" {
		_, _ = b.WriteString("|instructions=")
		_, _ = b.WriteString(instr)
	}
	if system := gjson.GetBytes(body, "system"); system.Exists() && system.Raw != "" && system.Raw != "null" {
		_, _ = b.WriteString("|system=")
		_, _ = b.WriteString(normalizeCompatSeedJSON(json.RawMessage(system.Raw)))
	}

	firstUserCaptured := false

	msgs := gjson.GetBytes(body, "messages")
	if msgs.Exists() && msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			role := msg.Get("role").String()
			switch role {
			case "system", "developer":
				_, _ = b.WriteString("|system=")
				if c := msg.Get("content"); c.Exists() {
					_, _ = b.WriteString(normalizeCompatSeedJSON(json.RawMessage(c.Raw)))
				}
			case "user":
				if !firstUserCaptured {
					_, _ = b.WriteString("|first_user=")
					if c := msg.Get("content"); c.Exists() {
						_, _ = b.WriteString(normalizeCompatSeedJSON(json.RawMessage(c.Raw)))
					}
					firstUserCaptured = true
				}
			}
			return true
		})
	} else if inp := gjson.GetBytes(body, "input"); inp.Exists() {
		if inp.Type == gjson.String {
			_, _ = b.WriteString("|input=")
			_, _ = b.WriteString(inp.String())
		} else if inp.IsArray() {
			inp.ForEach(func(_, item gjson.Result) bool {
				role := item.Get("role").String()
				switch role {
				case "system", "developer":
					_, _ = b.WriteString("|system=")
					if c := item.Get("content"); c.Exists() {
						_, _ = b.WriteString(normalizeCompatSeedJSON(json.RawMessage(c.Raw)))
					}
				case "user":
					if !firstUserCaptured {
						_, _ = b.WriteString("|first_user=")
						if c := item.Get("content"); c.Exists() {
							_, _ = b.WriteString(normalizeCompatSeedJSON(json.RawMessage(c.Raw)))
						}
						firstUserCaptured = true
					}
				}
				if !firstUserCaptured && item.Get("type").String() == "input_text" {
					_, _ = b.WriteString("|first_user=")
					if text := item.Get("text").String(); text != "" {
						_, _ = b.WriteString(text)
					}
					firstUserCaptured = true
				}
				return true
			})
		}
	}

	if b.Len() == 0 {
		return ""
	}
	return contentSessionSeedPrefix + b.String()
}

func deriveOpenAIPromptCacheBoostSeed(body []byte) (seed string, staticPrefix bool, staticBytes int) {
	staticSeed, bytes := deriveOpenAIPromptCacheStaticPrefixSeed(body)
	if staticSeed != "" && bytes >= openAIPromptCacheBoostMinStaticPrefixBytes {
		return staticSeed, true, bytes
	}
	return strings.TrimSpace(deriveOpenAIContentSessionSeed(body)), false, bytes
}

func deriveOpenAIPromptCacheBoostSeedAggressive(body []byte) (seed string, staticPrefix bool, staticBytes int) {
	staticSeed, bytes := deriveOpenAIPromptCacheStaticPrefixSeed(body)
	if staticSeed != "" {
		return staticSeed, true, bytes
	}
	seed = strings.TrimSpace(deriveOpenAIContentSessionSeed(body))
	return seed, false, bytes
}

func deriveOpenAIPromptCacheBoostSeedUpstreamPriority(body []byte) (seed string, staticPrefix bool, staticBytes int) {
	staticSeed, bytes := deriveOpenAIPromptCacheStaticPrefixSeedUpstreamPriority(body)
	if staticSeed != "" {
		return staticSeed, true, bytes
	}
	seed = strings.TrimSpace(deriveOpenAIContentSessionSeed(body))
	return seed, false, bytes
}

func openAIPromptCacheBoostBodyMayBenefitFromAggressive(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	return gjson.GetBytes(body, "system").Exists() ||
		gjson.GetBytes(body, "instructions").Exists() ||
		gjson.GetBytes(body, "tools").Exists() ||
		gjson.GetBytes(body, "functions").Exists() ||
		gjson.GetBytes(body, "messages").Exists() ||
		gjson.GetBytes(body, "input").Exists()
}

func deriveOpenAIPromptCacheStaticPrefixSeed(body []byte) (string, int) {
	return deriveOpenAIPromptCacheStaticPrefixSeedWithOptions(body, false)
}

func deriveOpenAIPromptCacheStaticPrefixSeedUpstreamPriority(body []byte) (string, int) {
	return deriveOpenAIPromptCacheStaticPrefixSeedWithOptions(body, true)
}

func deriveOpenAIPromptCacheStaticPrefixSeedOptimized(body []byte) (string, int) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "", 0
	}
	var b strings.Builder
	staticBytes := 0
	appendString := func(label, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		_, _ = b.WriteString("|")
		_, _ = b.WriteString(label)
		_, _ = b.WriteString("=")
		_, _ = b.WriteString(value)
		if label != "model" {
			staticBytes += len(value)
		}
	}
	appendJSON := func(label string, value gjson.Result) {
		if !value.Exists() || value.Raw == "" || value.Raw == "null" || value.Raw == "[]" || value.Raw == "{}" {
			return
		}
		appendString(label, normalizeCompatSeedJSON(json.RawMessage(value.Raw)))
	}
	appendString("model", gjson.GetBytes(body, "model").String())
	appendJSON("system", gjson.GetBytes(body, "system"))
	appendString("instructions", gjson.GetBytes(body, "instructions").String())
	appendJSON("tools", gjson.GetBytes(body, "tools"))
	appendJSON("functions", gjson.GetBytes(body, "functions"))
	if msgs := gjson.GetBytes(body, "messages"); msgs.Exists() && msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			role := strings.TrimSpace(msg.Get("role").String())
			if role == "system" || role == "developer" {
				appendJSON("message_"+role, msg.Get("content"))
			}
			return true
		})
	} else if input := gjson.GetBytes(body, "input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			role := strings.TrimSpace(item.Get("role").String())
			if role == "system" || role == "developer" {
				appendJSON("input_"+role, item.Get("content"))
			}
			return true
		})
	}
	if staticBytes == 0 || b.Len() == 0 {
		return "", staticBytes
	}
	return openAIPromptCacheBoostStaticSeedPrefix + strings.TrimPrefix(b.String(), "|"), staticBytes
}

func deriveOpenAIPromptCacheStaticPrefixSeedWithOptions(body []byte, upstreamPriority bool) (string, int) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return "", 0
	}

	var b strings.Builder
	staticBytes := 0
	appendString := func(label, value string, countStatic bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		_, _ = b.WriteString("|")
		_, _ = b.WriteString(label)
		_, _ = b.WriteString("=")
		_, _ = b.WriteString(value)
		if countStatic {
			staticBytes += len(value)
		}
	}
	appendJSON := func(label string, value gjson.Result, countStatic bool) {
		if !value.Exists() || value.Raw == "" || value.Raw == "null" || value.Raw == "[]" || value.Raw == "{}" {
			return
		}
		if upstreamPriority && openAIPromptCacheBoostSeedFieldIsDefault(label, value) {
			return
		}
		normalized := normalizeCompatSeedJSON(json.RawMessage(value.Raw))
		appendString(label, normalized, countStatic)
	}

	appendString("model", gjson.GetBytes(body, "model").String(), false)
	appendJSON("system", gjson.GetBytes(body, "system"), true)
	appendString("instructions", gjson.GetBytes(body, "instructions").String(), true)
	appendJSON("tools", gjson.GetBytes(body, "tools"), true)
	appendJSON("functions", gjson.GetBytes(body, "functions"), true)
	appendJSON("tool_choice", gjson.GetBytes(body, "tool_choice"), false)
	appendJSON("function_call", gjson.GetBytes(body, "function_call"), false)
	appendJSON("response_format", gjson.GetBytes(body, "response_format"), false)
	appendJSON("text", gjson.GetBytes(body, "text"), false)
	appendJSON("reasoning", gjson.GetBytes(body, "reasoning"), false)

	if msgs := gjson.GetBytes(body, "messages"); msgs.Exists() && msgs.IsArray() {
		msgs.ForEach(func(_, msg gjson.Result) bool {
			role := strings.TrimSpace(msg.Get("role").String())
			if role != "system" && role != "developer" {
				return true
			}
			appendJSON("message_"+role, msg.Get("content"), true)
			return true
		})
	} else if inp := gjson.GetBytes(body, "input"); inp.Exists() && inp.IsArray() {
		inp.ForEach(func(_, item gjson.Result) bool {
			role := strings.TrimSpace(item.Get("role").String())
			if role != "system" && role != "developer" {
				return true
			}
			appendJSON("input_"+role, item.Get("content"), true)
			return true
		})
	}

	if staticBytes == 0 || b.Len() == 0 {
		return "", staticBytes
	}
	return openAIPromptCacheBoostStaticSeedPrefix + strings.TrimPrefix(b.String(), "|"), staticBytes
}

func openAIPromptCacheBoostSeedFieldIsDefault(label string, value gjson.Result) bool {
	switch label {
	case "tool_choice", "function_call":
		return value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.String()), "auto")
	case "response_format":
		normalized := normalizeCompatSeedJSON(json.RawMessage(value.Raw))
		return normalized == `{"type":"text"}`
	default:
		return false
	}
}

func deriveOpenAIPromptCacheBoostLane(staticSeed string, staticBytes int, body []byte) (laneCount int, lane int) {
	laneCount = openAIPromptCacheBoostLaneCount(staticBytes)
	if laneCount <= 1 {
		return 1, 0
	}
	hashHex := hashSensitiveValueForLog(staticSeed)
	if len(hashHex) < 8 {
		return laneCount, 0
	}
	n, err := strconv.ParseUint(hashHex[:8], 16, 64)
	if err != nil {
		return laneCount, 0
	}
	return laneCount, int(n % uint64(laneCount))
}

func openAIPromptCacheBoostLaneCount(staticBytes int) int {
	switch {
	case staticBytes >= openAIPromptCacheBoostHugeStaticPrefixBytes:
		return openAIPromptCacheBoostMaxLaneCount
	case staticBytes >= openAIPromptCacheBoostLargeStaticPrefixBytes:
		return 8
	case staticBytes >= openAIPromptCacheBoostMediumStaticPrefixBytes:
		return 4
	case staticBytes >= openAIPromptCacheBoostMinStaticPrefixBytes:
		return 2
	default:
		return 1
	}
}

// DeriveOpenAIPromptCacheBoostAffinityHash returns a sticky-session hash for
// prompt-cache affinity. It is only emitted when the request has a substantial
// static prefix; ordinary content-derived sessions keep the existing path.
func DeriveOpenAIPromptCacheBoostAffinityHash(model string, body []byte) string {
	seed, staticPrefix, staticBytes := deriveOpenAIPromptCacheBoostSeed(body)
	if !staticPrefix || seed == "" {
		return ""
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	if normalizedModel == "" {
		normalizedModel = "unknown"
	}
	laneCount, lane := deriveOpenAIPromptCacheBoostLane(seed, staticBytes, body)
	return openAIPromptCacheBoostAffinitySessionPrefix + hashSensitiveValueForLog(
		strings.Join([]string{
			"strategy", "static-prefix-v2",
			"model", normalizedModel,
			"seed", seed,
			"lane", strconv.Itoa(lane),
			"lanes", strconv.Itoa(laneCount),
		}, "|"),
	)
}

func IsOpenAIPromptCacheBoostAffinitySessionHash(sessionHash string) bool {
	normalized := strings.TrimSpace(sessionHash)
	return strings.HasPrefix(normalized, openAIPromptCacheBoostAffinitySessionPrefix) ||
		strings.HasPrefix(normalized, openAIPromptCacheBoostAggressiveAffinitySessionPrefix) ||
		strings.HasPrefix(normalized, openAIPromptCacheBoostUpstreamAffinitySessionPrefix) ||
		strings.HasPrefix(normalized, openAIPromptCacheBoostOptimizedAffinitySessionPrefix)
}

func IsOpenAIPromptCacheBoostAggressiveAffinitySessionHash(sessionHash string) bool {
	return strings.HasPrefix(strings.TrimSpace(sessionHash), openAIPromptCacheBoostAggressiveAffinitySessionPrefix)
}

func IsOpenAIPromptCacheBoostUpstreamAffinitySessionHash(sessionHash string) bool {
	return strings.HasPrefix(strings.TrimSpace(sessionHash), openAIPromptCacheBoostUpstreamAffinitySessionPrefix)
}

func IsOpenAIPromptCacheBoostOptimizedAffinitySessionHash(sessionHash string) bool {
	return strings.HasPrefix(strings.TrimSpace(sessionHash), openAIPromptCacheBoostOptimizedAffinitySessionPrefix)
}

func DeriveOpenAIPromptCacheBoostAggressiveAffinityHash(model string, body []byte) string {
	seed, staticBytes := deriveOpenAIPromptCacheStaticPrefixSeed(body)
	if seed == "" {
		return ""
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	if normalizedModel == "" {
		normalizedModel = "unknown"
	}
	return openAIPromptCacheBoostAggressiveAffinitySessionPrefix + hashSensitiveValueForLog(
		strings.Join([]string{
			"strategy", "aggressive-static-prefix-v1",
			"model", normalizedModel,
			"seed", seed,
			"static_bytes", strconv.Itoa(staticBytes),
		}, "|"),
	)
}

func DeriveOpenAIPromptCacheBoostUpstreamAffinityHash(model string, body []byte) string {
	seed, staticBytes := deriveOpenAIPromptCacheStaticPrefixSeedUpstreamPriority(body)
	if seed == "" {
		return ""
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	if normalizedModel == "" {
		normalizedModel = "unknown"
	}
	return openAIPromptCacheBoostUpstreamAffinitySessionPrefix + hashSensitiveValueForLog(
		strings.Join([]string{
			"strategy", "upstream-static-prefix-v1",
			"model", normalizedModel,
			"seed", seed,
			"static_bytes", strconv.Itoa(staticBytes),
		}, "|"),
	)
}

func DeriveOpenAIPromptCacheBoostOptimizedAffinityHash(model string, body []byte) string {
	seed, staticBytes := deriveOpenAIPromptCacheStaticPrefixSeedOptimized(body)
	if seed == "" {
		return ""
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	if normalizedModel == "" {
		normalizedModel = "unknown"
	}
	return openAIPromptCacheBoostOptimizedAffinitySessionPrefix + hashSensitiveValueForLog(
		strings.Join([]string{
			"strategy", "upstream-static-prefix-v3",
			"model", normalizedModel,
			"seed", seed,
			"static_bytes", strconv.Itoa(staticBytes),
		}, "|"),
	)
}

func DeriveOpenAIPromptCacheBoostExplicitKeyUpstreamAffinityHash(model string, promptCacheKey string) string {
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	if promptCacheKey == "" {
		return ""
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		normalizedModel = "unknown"
	}
	return openAIPromptCacheBoostUpstreamAffinitySessionPrefix + hashSensitiveValueForLog(
		strings.Join([]string{
			"strategy", "upstream-explicit-prompt-cache-key-v1",
			"model", normalizedModel,
			"prompt_cache_key", promptCacheKey,
		}, "|"),
	)
}

func deriveOpenAIVirtualPromptCacheKey(account *Account, model string, body []byte) string {
	if account == nil || len(body) == 0 || !account.IsOpenAIPromptCacheBoostEnabled() {
		return ""
	}
	seed, staticPrefix, staticBytes := deriveOpenAIPromptCacheBoostSeed(body)
	keyPrefix := "nuro-pcache-"
	strategy := "static-prefix-v2"
	if account.IsOpenAIPromptCacheKeyOptimizationEnabled() {
		seed, staticBytes = deriveOpenAIPromptCacheStaticPrefixSeedOptimized(body)
		staticPrefix = seed != ""
		if seed == "" {
			seed = strings.TrimSpace(deriveOpenAIContentSessionSeed(body))
		}
		keyPrefix = "nuro-pcache-v3-"
		strategy = "static-prefix-v3"
	} else if account.IsOpenAIPromptCacheBoostUpstreamHitPriorityEnabled() {
		seed, staticPrefix, staticBytes = deriveOpenAIPromptCacheBoostSeedUpstreamPriority(body)
	} else if account.IsOpenAIPromptCacheBoostAggressive() {
		seed, staticPrefix, staticBytes = deriveOpenAIPromptCacheBoostSeedAggressive(body)
	}
	if seed == "" {
		return ""
	}
	normalizedModel := strings.TrimSpace(model)
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	if normalizedModel == "" {
		normalizedModel = "unknown"
	}
	laneCount, lane := 1, 0
	if staticPrefix {
		laneCount, lane = deriveOpenAIPromptCacheBoostLane(seed, staticBytes, body)
	}
	return keyPrefix + hashSensitiveValueForLog(
		strings.Join([]string{
			"strategy", strategy,
			"account", strconv.FormatInt(account.ID, 10),
			"model", normalizedModel,
			"seed", seed,
			"lane", strconv.Itoa(lane),
			"lanes", strconv.Itoa(laneCount),
		}, "|"),
	)
}

func deriveOpenAIPromptCacheBoostAffinityHashForAccount(account *Account, model string, body []byte) string {
	if account == nil || !account.IsOpenAIPromptCacheBoostEnabled() {
		return ""
	}
	if account.IsOpenAIPromptCacheKeyOptimizationEnabled() {
		return DeriveOpenAIPromptCacheBoostOptimizedAffinityHash(model, body)
	}
	if account.IsOpenAIPromptCacheBoostUpstreamHitPriorityEnabled() {
		return DeriveOpenAIPromptCacheBoostUpstreamAffinityHash(model, body)
	}
	if account.IsOpenAIPromptCacheBoostAggressive() {
		return DeriveOpenAIPromptCacheBoostAggressiveAffinityHash(model, body)
	}
	return DeriveOpenAIPromptCacheBoostAffinityHash(model, body)
}
