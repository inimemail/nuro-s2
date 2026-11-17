package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0222Gemini4DateAndOverrides(t *testing.T) {
	boundary := gemini4CarbonStandardPriceStart
	base := &ModelPricing{InputPricePerToken: 99, OutputPricePerToken: 99, CacheReadPricePerToken: 99}
	before := applyGemini4OfficialPricing("models/gemini-4-carbon-medium", base, boundary.Add(-time.Nanosecond))
	after := applyGemini4OfficialPricing("gemini-4-carbon", base, boundary)
	require.InDelta(t, 2e-6, before.InputPricePerToken, 1e-15)
	require.InDelta(t, 4e-6, after.InputPricePerToken, 1e-15)
	require.InDelta(t, 36e-6, after.OutputPricePerTokenPriority, 1e-15)
	require.Equal(t, 99.0, base.InputPricePerToken)
	explicit := &ModelPricing{PricingOverrideInput: true, PricingOverrideOutput: true, PricingOverrideCacheRead: true}
	require.Equal(t, explicit, applyGemini4OfficialPricing("gemini-4-carbon", explicit, boundary))
	require.Same(t, base, applyGemini4OfficialPricing("gemini-4-carbon-unrecognized", base, boundary))
	hydrogen := applyGemini4OfficialPricing("gemini-4-hydrogen", base, boundary)
	require.InDelta(t, .075e-6, hydrogen.CacheReadPricePerToken, 1e-15)
	billing := &BillingService{}
	_, _, err := billing.GeminiCacheStoragePrice("gemini-4-carbon", boundary)
	require.Error(t, err)
}

func TestV0222VectorFallbackExactModels(t *testing.T) {
	for _, family := range []string{"Embedding", "Reranker"} {
		for _, item := range []struct {
			tier  string
			price float64
		}{{"8B", .04e-6}, {"4B", .02e-6}, {"0.6B", .01e-6}} {
			p := qwen3VectorFallback("Qwen/Qwen3-" + family + "-" + item.tier)
			require.NotNil(t, p)
			require.Equal(t, item.price, p.InputPricePerToken)
			require.Zero(t, p.OutputPricePerToken)
		}
	}
	for _, name := range []string{"Qwen3-VL-Embedding-8B", "Qwen3-Embedding-2B", "Qwen3-Reranker-8B-fake"} {
		require.Nil(t, qwen3VectorFallback(name))
	}
}

func TestV0222VertexRegionalRoutingAndExplicitPrices(t *testing.T) {
	account := &Account{Platform: PlatformGemini, Type: AccountTypeServiceAccount, Credentials: map[string]any{"location": "eu", "project_id": "fixture"}}
	require.Equal(t, "eu", account.VertexLocation("gemini-3.6-flash"))
	require.Equal(t, "global", account.VertexLocation("gemini-4-carbon"))
	u, err := buildVertexGeminiAccountURL(account, "gemini-3.6-flash", "generateContent", false)
	require.NoError(t, err)
	require.Contains(t, u, "https://aiplatform.eu.rep.googleapis.com/")
	require.Equal(t, 1.1, VertexUpstreamPriceMultiplier(account, "gemini-3.6-flash", ""))
	require.Equal(t, 1.0, VertexUpstreamPriceMultiplier(account, "gemini-4-carbon", ""))
	bound, err := GeminiCacheBoundAccount(account, "projects/fixture/locations/us/cachedContents/old")
	require.NoError(t, err)
	require.Equal(t, "us", bound.VertexLocation("gemini-4-carbon"))
	require.Equal(t, "eu", account.VertexLocation("gemini-3.6-flash"))
	p := &ModelPricing{InputPricePerToken: 0, PricingOverrideInput: true, OutputPricePerToken: 2, CacheReadPricePerToken: 3}
	adjusted := applyVertexDefaultTokenPricing(p, 1.1)
	require.Zero(t, adjusted.InputPricePerToken)
	require.Equal(t, 2.2, adjusted.OutputPricePerToken)
	require.Equal(t, 2.0, p.OutputPricePerToken)
	account.Credentials["auth_mode"] = "apikey"
	delete(account.Credentials, "project_id")
	require.Equal(t, 1.0, VertexUpstreamPriceMultiplier(account, "gemini-3.6-flash", "us"))
}

func TestV0222SchemaRegexPreservesPythonLookaroundAndInstanceData(t *testing.T) {
	input := []byte(`{"tools":[{"type":"function","parameters":{"type":"object","properties":{"valid":{"type":"string","pattern":"(?<=a)b(?=c)"},"foreign":{"type":"string","pattern":"(?<name>x)","default":{"pattern":"(?<name>literal)"}}},"patternProperties":{"\\p{L}":{"type":"string"}},"additionalProperties":{"type":"integer"},"examples":[{"pattern":"(?<name>literal)"}]}}],"input":[{"role":"user","content":"(?<name>literal)"}]}`)
	out, changed, err := sanitizeOpenAIToolSchemaPatterns(input)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "(?<=a)b(?=c)", gjson.GetBytes(out, "tools.0.parameters.properties.valid.pattern").String())
	require.False(t, gjson.GetBytes(out, "tools.0.parameters.properties.foreign.pattern").Exists())
	require.Equal(t, "(?<name>literal)", gjson.GetBytes(out, "tools.0.parameters.properties.foreign.default.pattern").String())
	require.Equal(t, `{"type":"integer"}`, gjson.GetBytes(out, "tools.0.parameters.additionalProperties").Raw)
	require.Equal(t, gjson.GetBytes(input, "input").Raw, gjson.GetBytes(out, "input").Raw)
	out2, changed, err := sanitizeOpenAIToolSchemaPatterns(out)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, out, out2)
}

func TestV0222SchemaRegexPreservesExactNumbers(t *testing.T) {
	input := []byte(`{"tools":[{"type":"function","parameters":{"properties":{"tag":{"type":"string","pattern":"(?<name>x)"},"id":{"type":"integer","enum":[9007199254740993],"default":9007199254740993,"minimum":9007199254740993},"ratio":{"type":"number","const":0.12345678901234567890123456789}},"examples":[{"id":9007199254740993}]}}]}`)
	out, changed, err := sanitizeOpenAIToolSchemaPatterns(input)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(out, "tools.0.parameters.properties.tag.pattern").Exists())
	for _, path := range []string{"properties.id.enum.0", "properties.id.default", "properties.id.minimum", "properties.ratio.const", "examples.0.id"} {
		fullPath := "tools.0.parameters." + path
		require.Equal(t, gjson.GetBytes(input, fullPath).Raw, gjson.GetBytes(out, fullPath).Raw, path)
	}
}

func TestV0222AdditionalOpaqueDeclarationReference(t *testing.T) {
	input := []any{map[string]any{"type": "additional_tools", "id": "opaque_declaration"}, map[string]any{"type": "item_reference", "id": "opaque_declaration"}, map[string]any{"type": "item_reference", "id": "old_response"}}
	filtered := filterCodexInputWithOptions(input, codexInputFilterOptions{})
	require.Len(t, filtered, 2)
	ref, ok := filtered[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "opaque_declaration", ref["id"])
}

func TestV0222AdditionalDeclarationCallPrefixIsOpaque(t *testing.T) {
	for _, preserveReferences := range []bool{false, true} {
		input := []any{
			map[string]any{"type": "additional_tools", "id": "call_opaque_declaration"},
			map[string]any{"type": "item_reference", "id": "call_opaque_declaration"},
			map[string]any{"type": "function_call_output", "call_id": "call_opaque_result", "output": "result"},
		}
		filtered := filterCodexInputWithOptions(input, codexInputFilterOptions{PreserveReferences: preserveReferences, PreserveCallIDs: true})
		require.Len(t, filtered, 3)
		for _, index := range []int{0, 1} {
			item, ok := filtered[index].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "call_opaque_declaration", item["id"], "references must retain their declaration's identity")
		}
		output, ok := filtered[2].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "call_opaque_result", output["call_id"])
	}
}

func TestV0222ResponseFormatValidationAndToolCoexistence(t *testing.T) {
	require.Error(t, validateGeminiChatResponseFormat(json.RawMessage(`{"type":"json_schema","json_schema":{}}`), "gemini-4-carbon"))
	format := json.RawMessage(`{"type":"json_schema","json_schema":{"schema":{"type":"object","additionalProperties":false}}}`)
	out := applyGeminiChatResponseFormat([]byte(`{"tools":[{"functionDeclarations":[]}],"generationConfig":{"temperature":1}}`), format, "gemini-4-carbon")
	require.Equal(t, "application/json", gjson.GetBytes(out, "generationConfig.responseMimeType").String())
	require.False(t, gjson.GetBytes(out, "generationConfig.responseJsonSchema.additionalProperties").Bool())
	require.Equal(t, int64(1), gjson.GetBytes(out, "generationConfig.temperature").Int())
}

func TestV0222AuthNilIdentity(t *testing.T) {
	var svc *AuthService
	require.False(t, svc.shouldApplyEmailFirstBindDefaults(context.Background(), 1, nil, true))
	require.False(t, svc.shouldApplyEmailFirstBindDefaults(context.Background(), 1, &dbent.AuthIdentity{}, false))
}

func TestV0222BedrockClassificationIsNarrow(t *testing.T) {
	require.True(t, isBedrockAccountBlockedMessage("Operation not allowed."))
	require.False(t, isBedrockAccountBlockedMessage("Invalid field: Operation not allowed"))
	require.True(t, isBedrockContentFilterError(503, []byte(`{"message":"Blocked by content filtering policy"}`)))
	require.False(t, isBedrockContentFilterError(400, []byte(`{"message":"Blocked by content filtering policy"}`)))
}

func TestV0222GeminiToolBoundaryPreservesIdenticalIndependentCalls(t *testing.T) {
	var b geminiToolCallBoundary
	fc := map[string]any{"name": "read", "args": map[string]any{"x": 1}}
	b.observe(fc)
	require.True(t, b.startsNewBlock("read", `{"x":1}`, `{"x":1}`, fc))
	b.observe(map[string]any{"name": "read", "id": "a", "args": `{"x":`})
	require.False(t, b.startsNewBlock("read", `{"x":`, `1}`, map[string]any{"name": "read", "id": "a", "args": "1}"}))
	require.True(t, b.startsNewBlock("read", `{"x":1}`, `{"x":1}`, map[string]any{"name": "read", "id": "b"}))
}

func TestV0222AnthropicEffortPolicyPreservesRequestedAndMapsClientAlias(t *testing.T) {
	for _, requested := range []string{"max", "none"} {
		body := []byte(`{"model":"public-claude","reasoning":{"effort":"` + requested + `"},"input":"hello"}`)
		ctx := WithRequestedReasoningEffort(context.Background(), requested)
		adjusted, changed, err := ApplyOpenAIReasoningEffortPolicyWithAction(body, "medium", []ReasoningEffortMapping{{From: "none", To: "high", Model: "public-claude", MatchType: "exact"}}, "downgrade")
		require.NoError(t, err)
		require.True(t, changed)
		var req apicompat.ResponsesRequest
		require.NoError(t, json.Unmarshal(adjusted, &req))
		// Model-scoped policy has already evaluated the public name. Account
		// mapping happens afterward, as in the production compatibility path.
		req.Model = "claude-sonnet-4-6"
		converted, err := apicompat.ResponsesToAnthropicRequest(&req)
		require.NoError(t, err)
		require.NotNil(t, converted.OutputConfig)
		require.Equal(t, "medium", converted.OutputConfig.Effort)
		require.Equal(t, requested, *RequestedReasoningEffortFromContext(ctx))
		unchanged, changed, err := ApplyOpenAIReasoningEffortPolicyWithAction(body, "", nil, "")
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, unchanged)
	}
}
