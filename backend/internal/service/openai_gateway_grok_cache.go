package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokConversationIDHeader        = "X-Grok-Conv-Id"
	grokFreeCacheNativeToolsJSON    = `[{"type":"web_search"},{"type":"x_search"}]`
	grokFreeCacheDisabledToolChoice = "none"
	grokFreeRolling24hTokenLimit    = int64(2_000_000)
)

// resolveGrokCacheIdentity derives one stable, tenant-isolated routing identity
// for xAI's server-side prompt cache. The returned value is safe to expose to
// the upstream: it never contains the client's raw session identifier.
//
// A valid downstream API key is required. This intentionally fails closed on
// internal probes and incomplete request contexts instead of creating a cache
// identity that could be shared by unrelated tenants.
func resolveGrokCacheIdentity(c *gin.Context, body []byte, explicitKey, upstreamModel string) string {
	apiKeyID := getAPIKeyIDFromContext(c)
	if apiKeyID <= 0 {
		return ""
	}
	// /responses/compact rejects tool_choice and does not represent a normal
	// conversation turn. Keep both cache identity and Free-tier routing
	// augmentation out of this path.
	if isOpenAIResponsesCompactPath(c) {
		return ""
	}

	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if model == "" {
		return ""
	}

	seed := explicitGrokCacheSeed(c, body, explicitKey)
	if seed == "" {
		// Reuse the fork's cache-enhancement static-prefix derivation. It keeps
		// stable system/tool prefixes together without changing OpenAI routing.
		seed, _ = deriveOpenAIPromptCacheStaticPrefixSeedOptimized(body)
		if seed == "" && grokCacheBodyHasUserAnchor(body) {
			seed = deriveOpenAIContentSessionSeed(body)
		}
	}
	if seed == "" {
		return ""
	}

	// generateSessionUUID hashes the whole seed before formatting it as a UUID.
	// Include a versioned namespace so this identity cannot collide with other
	// upstream session identifiers derived by sub2api.
	isolatedSeed := fmt.Sprintf("grok-prompt-cache:v1:%d:%s:%s", apiKeyID, model, seed)
	return generateSessionUUID(isolatedSeed)
}

func explicitGrokCacheSeed(c *gin.Context, body []byte, explicitKey string) string {
	seed := ""
	if c != nil && c.Request != nil {
		seed = strings.TrimSpace(c.GetHeader("session_id"))
		if seed == "" {
			seed = strings.TrimSpace(c.GetHeader("conversation_id"))
		}
		if seed == "" {
			seed = strings.TrimSpace(c.GetHeader(grokConversationIDHeader))
		}
	}
	if seed == "" && len(body) > 0 {
		seed = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	return seed
}

func isGrokRequestContext(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, exists := c.Get("api_key")
	if !exists {
		return false
	}
	apiKey, ok := v.(*APIKey)
	return ok && apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == PlatformGrok
}

// applyGrokResponsesCacheIdentity writes the cache routing identity into an
// xAI Responses request. Existing client values are deliberately replaced by
// the tenant-isolated value to prevent collisions on shared OAuth accounts.
//
// Free OAuth requests without native search tools are routed by xAI to the
// non-cacheable build-free model. For otherwise tool-free requests, add the
// native tools with tool_choice=none: this selects the cache-capable tier
// without allowing an actual search. Explicit client tools are handled by the
// narrower Messages-only mixed-tools policy below.
func applyGrokResponsesCacheIdentity(body, intentSourceBody []byte, identity string, injectFreeTierTools bool) ([]byte, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		if gjson.GetBytes(body, "prompt_cache_key").Exists() {
			return sjson.DeleteBytes(body, "prompt_cache_key")
		}
		return body, nil
	}
	out, err := sjson.SetBytes(body, "prompt_cache_key", identity)
	if err != nil {
		return nil, err
	}
	if !injectFreeTierTools {
		return out, nil
	}
	// Inspect the pre-sanitization source. patchGrokResponsesBody may remove an
	// unsupported client tool and its tool_choice; that must not turn an
	// explicit client tool intent into an eligible native-tool request.
	if gjson.GetBytes(intentSourceBody, "tools").Exists() || gjson.GetBytes(intentSourceBody, "tool_choice").Exists() {
		return out, nil
	}
	out, err = sjson.SetRawBytes(out, "tools", []byte(grokFreeCacheNativeToolsJSON))
	if err != nil {
		return nil, err
	}
	return sjson.SetBytes(out, "tool_choice", grokFreeCacheDisabledToolChoice)
}

// applyGrokFreeMessagesFunctionToolCacheRoute enables xAI's cache-capable
// mixed-tools route for supported Responses/WS/Messages ingress only when the
// selected account is known to be Free. Native tools become eligible under
// auto selection, so every caller must preserve the tier and tenant-identity
// gates rather than applying the route to paid or unknown accounts.
func applyGrokFreeMessagesFunctionToolCacheRoute(body, intentSourceBody []byte, account *Account, cacheIdentity string) ([]byte, error) {
	if strings.TrimSpace(cacheIdentity) == "" || !isKnownGrokFreeAccount(account) {
		return body, nil
	}
	intentTools := gjson.GetBytes(intentSourceBody, "tools")
	intentToolChoice := gjson.GetBytes(intentSourceBody, "tool_choice")
	if !isGrokFreeCacheFunctionToolIntent(intentTools, intentToolChoice) {
		return body, nil
	}
	// xAI's Grok Build client declares its native search tools as Responses
	// function entries named web_search/x_search. Converting those reserved
	// declarations avoids duplicate native tool names after augmentation, but
	// the same names are valid custom function names for ordinary models. Keep
	// ordinary-model requests byte-for-byte unchanged rather than dropping a
	// caller's function schema.
	if grokFreeCacheFunctionToolsContainReservedName(intentTools) && !isGrokBuildFunctionToolModel(intentSourceBody) {
		return body, nil
	}
	return appendMissingGrokFreeCacheNativeTools(body)
}

func grokFreeCacheFunctionToolsContainReservedName(tools gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "function") &&
			isGrokFreeCacheReservedSearchName(tool.Get("name").String()) {
			return true
		}
	}
	return false
}

func isGrokFreeCacheReservedSearchName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web_search", "x_search":
		return true
	default:
		return false
	}
}

func isGrokBuildFunctionToolModel(body []byte) bool {
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "model").String())) {
	case "grok-build", "grok-build-latest", "grok-build-0.1":
		return true
	default:
		return false
	}
}

func isKnownGrokFreeAccount(account *Account) bool {
	if account == nil || !account.IsGrokOAuth() {
		return false
	}
	freeSignal := false
	paidSignal := false
	inferredFreeSignal := false
	if billing, err := grokBillingSnapshotFromExtra(account.Extra); err == nil && billing != nil {
		if tier := strings.TrimSpace(billing.Plan); tier != "" {
			if isGrokFreeSubscriptionTier(tier) {
				freeSignal = true
			} else if !isGrokUnknownSubscriptionTier(tier) {
				paidSignal = true
			}
		}
		if billing.UsagePercent != nil || billing.UsedPercent != nil ||
			(billing.MonthlyLimitCents != nil && *billing.MonthlyLimitCents > 0) {
			paidSignal = true
		}
		// xAI deliberately reports an empty plan for Free accounts; only paid
		// subscriptions receive a SuperGrok plan/monthly limit. A successful
		// monthly billing observation with no paid signal is therefore positive
		// Free evidence, not an unknown tier. Keep partial probes fail-closed.
		if !billing.Partial && len(billing.FailedWindows) == 0 &&
			(strings.TrimSpace(billing.MonthlyUpdatedAt) != "" ||
				(billing.StatusCode >= http.StatusOK && billing.StatusCode < http.StatusMultipleChoices)) {
			inferredFreeSignal = true
		}
	}
	if snapshot, err := grokQuotaSnapshotFromExtra(account.Extra); err == nil && snapshot != nil {
		if tier := strings.TrimSpace(snapshot.SubscriptionTier); tier != "" {
			if isGrokFreeSubscriptionTier(tier) {
				freeSignal = true
			} else if !isGrokUnknownSubscriptionTier(tier) {
				paidSignal = true
			}
		}
		if snapshot.Tokens != nil && snapshot.Tokens.Limit != nil &&
			*snapshot.Tokens.Limit == grokFreeRolling24hTokenLimit {
			inferredFreeSignal = true
		}
	}
	if tier := strings.TrimSpace(account.GetCredential("subscription_tier")); tier != "" {
		if isGrokFreeSubscriptionTier(tier) {
			freeSignal = true
		} else if !isGrokUnknownSubscriptionTier(tier) {
			paidSignal = true
		}
	}
	// Explicit paid evidence always wins over an inferred Free signal. This
	// protects upgraded/stale accounts whose previous quota snapshot still
	// carries the historical 2M Free token limit.
	return !paidSignal && (freeSignal || inferredFreeSignal)
}

func isGrokFreeSubscriptionTier(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "free", "grok-free", "grok_free", "free-tier", "free_tier", "basic", "grok-basic", "grok_basic":
		return true
	default:
		return false
	}
}

func isGrokUnknownSubscriptionTier(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "", "unknown", "n/a", "none":
		return true
	default:
		return false
	}
}

func isGrokFreeCacheFunctionToolIntent(tools, toolChoice gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	items := tools.Array()
	if len(items) == 0 {
		return false
	}
	for _, tool := range items {
		if !tool.IsObject() || strings.TrimSpace(tool.Get("type").String()) != "function" {
			return false
		}
		// Responses function declarations keep name at the top level. Reject
		// Chat Completions' nested function shape and incomplete declarations.
		if strings.TrimSpace(tool.Get("name").String()) == "" || tool.Get("function").Exists() {
			return false
		}
	}
	if !toolChoice.Exists() {
		return true
	}
	return toolChoice.Type == gjson.String && strings.TrimSpace(toolChoice.String()) == "auto"
}

func appendMissingGrokFreeCacheNativeTools(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body, nil
	}

	items := tools.Array()
	if len(items) == 0 {
		return body, nil
	}
	merged := make([]json.RawMessage, 0, len(items)+2)
	present := make(map[string]bool, 2)
	hasFunction := false
	for _, tool := range items {
		toolType := strings.TrimSpace(tool.Get("type").String())
		switch toolType {
		case "function":
			name := strings.TrimSpace(tool.Get("name").String())
			if !tool.IsObject() || name == "" || tool.Get("function").Exists() {
				return body, nil
			}
			// Grok Build may send web_search/x_search as function declarations.
			// Convert those two reserved names to native entries and deduplicate;
			// arbitrary function tools remain unchanged.
			if isGrokFreeCacheReservedSearchName(name) {
				reservedName := strings.ToLower(name)
				if present[reservedName] {
					continue
				}
				raw, err := json.Marshal(map[string]string{"type": reservedName})
				if err != nil {
					return nil, err
				}
				merged = append(merged, raw)
				present[reservedName] = true
				continue
			}
			hasFunction = true
			merged = append(merged, json.RawMessage(tool.Raw))
		case "web_search", "x_search":
			if present[toolType] {
				continue
			}
			merged = append(merged, json.RawMessage(tool.Raw))
			present[toolType] = true
		default:
			return body, nil
		}
	}
	if !hasFunction {
		return body, nil
	}
	for _, toolType := range []string{"web_search", "x_search"} {
		if present[toolType] {
			continue
		}
		raw, err := json.Marshal(map[string]string{"type": toolType})
		if err != nil {
			return nil, err
		}
		merged = append(merged, raw)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", encoded)
}

// applyGrokCacheHeaders applies the documented Chat Completions conversation
// routing header. The request is built from a fresh header map, so client
// supplied x-grok headers cannot override this server-derived value.
func applyGrokCacheHeaders(headers http.Header, identity string) {
	if headers == nil {
		return
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		headers.Del(grokConversationIDHeader)
		return
	}
	headers.Set(grokConversationIDHeader, identity)
}

// stripGrokChatPromptCacheKey removes the Responses-only body field after it
// has been used as an identity seed. Chat Completions routes cache by header.
func stripGrokChatPromptCacheKey(body []byte) ([]byte, error) {
	if !gjson.GetBytes(body, "prompt_cache_key").Exists() {
		return body, nil
	}
	return sjson.DeleteBytes(body, "prompt_cache_key")
}

func grokCacheBodyHasUserAnchor(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		anchored := false
		messages.ForEach(func(_, message gjson.Result) bool {
			if strings.TrimSpace(message.Get("role").String()) != "user" {
				return true
			}
			anchored = grokCacheContentIsMeaningful(message.Get("content"))
			return false
		})
		return anchored
	}
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		return strings.TrimSpace(input.String()) != ""
	}
	if !input.IsArray() {
		return false
	}
	anchored := false
	input.ForEach(func(_, item gjson.Result) bool {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "input_text":
			anchored = strings.TrimSpace(item.Get("text").String()) != ""
		default:
			if strings.TrimSpace(item.Get("role").String()) == "user" {
				anchored = grokCacheContentIsMeaningful(item.Get("content"))
			}
		}
		return !anchored
	})
	return anchored
}

func grokCacheContentIsMeaningful(content gjson.Result) bool {
	if !content.Exists() || content.Type == gjson.Null {
		return false
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String()) != ""
	}
	if content.IsArray() {
		meaningful := false
		content.ForEach(func(_, item gjson.Result) bool {
			if item.Type == gjson.String {
				meaningful = strings.TrimSpace(item.String()) != ""
			} else if text := item.Get("text"); text.Exists() {
				meaningful = strings.TrimSpace(text.String()) != ""
			} else {
				meaningful = strings.TrimSpace(item.Raw) != "" && item.Raw != "null" && item.Raw != "{}" && item.Raw != "[]"
			}
			return !meaningful
		})
		return meaningful
	}
	return strings.TrimSpace(content.Raw) != "" && content.Raw != "null" && content.Raw != "{}"
}
