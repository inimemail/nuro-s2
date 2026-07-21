package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func promptCacheCreationOptimizationAccount(accountType string, enabled bool, mode string) *Account {
	credentials := map[string]any{}
	if enabled {
		credentials["openai_prompt_cache_creation_optimization_enabled"] = true
	}
	if mode != "" {
		credentials["openai_prompt_cache_creation_optimization_mode"] = mode
	}
	return &Account{Platform: PlatformOpenAI, Type: accountType, Credentials: credentials}
}

func TestOpenAIPromptCacheCreationOptimization_DisabledIsByteExactNoOp(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_retention":"24h","input":[{"role":"developer","content":"stable"}]}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, false, "")

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Equal(t, body, updated)
	require.Equal(t, &body[0], &updated[0], "disabled path must return the original byte slice")
}

func TestOpenAIPromptCacheCreationOptimization_IsolatedPerAccount(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_retention":"24h","input":[{"role":"developer","content":"stable"}]}`)
	enabledAccount := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	disabledAccount := promptCacheCreationOptimizationAccount(AccountTypeOAuth, false, "")

	enabledBody, enabledResult, err := applyOpenAIPromptCacheCreationOptimizationBody(enabledAccount, "gpt-5.6-sol", body)
	require.NoError(t, err)
	require.True(t, enabledResult.Applied)
	require.NotEqual(t, body, enabledBody)
	require.Equal(t, "explicit", gjson.GetBytes(enabledBody, "prompt_cache_options.mode").String())

	disabledBody, disabledResult, err := applyOpenAIPromptCacheCreationOptimizationBody(disabledAccount, "gpt-5.6-sol", body)
	require.NoError(t, err)
	require.False(t, disabledResult.Applied)
	require.Equal(t, body, disabledBody)
	require.Equal(t, &body[0], &disabledBody[0], "an enabled peer account must not affect the disabled account path")
	require.Equal(t, "24h", gjson.GetBytes(disabledBody, "prompt_cache_retention").String())
	require.False(t, gjson.GetBytes(disabledBody, "prompt_cache_options").Exists())
}

func TestOpenAIPromptCacheCreationOptimization_DefaultModeAReducesCacheCreation(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_retention":"24h","input":"hello"}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, "")

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.False(t, result.BreakpointInserted)
	require.False(t, gjson.GetBytes(updated, "prompt_cache_retention").Exists())
	require.Equal(t, "explicit", gjson.GetBytes(updated, "prompt_cache_options.mode").String())
	require.Equal(t, "24h", gjson.GetBytes(updated, "prompt_cache_options.ttl").String())
}

func TestOpenAIPromptCacheCreationOptimization_NonGPT56IsNoOp(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","prompt_cache_retention":"24h","input":"hello"}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.5", body)

	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Equal(t, body, updated)
}

func TestOpenAIPromptCacheCreationOptimization_DoesNotTouchImageRequests(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"generate"}],"tools":[{"type":"image_generation"}]}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Equal(t, body, updated)
	require.Equal(t, &body[0], &updated[0])
}

func TestOpenAIPromptCacheCreationOptimization_PassiveImageNamespaceStillApplies(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_retention":"24h","tools":[{"type":"namespace","name":"image_gen"}],"input":[{"type":"additional_tools","tools":[{"type":"image_generation"}]},{"role":"user","content":"write code"}],"tool_choice":"auto"}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.False(t, gjson.GetBytes(updated, "prompt_cache_retention").Exists())
	require.Equal(t, "24h", gjson.GetBytes(updated, "prompt_cache_options.ttl").String())
}

func TestOpenAIPromptCacheCreationOptimization_UsesIngressIntentBeforeServerToolInjection(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	injectedBody := []byte(`{"model":"gpt-5.6-sol","input":"write code","tools":[{"type":"image_generation"}]}`)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(
		account,
		"gpt-5.6-sol",
		injectedBody,
		false,
	)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.Equal(t, "explicit", gjson.GetBytes(updated, "prompt_cache_options.mode").String())
	require.True(t, gjson.GetBytes(updated, `tools.#(type=="image_generation")`).Exists())

	textBody := []byte(`{"model":"gpt-5.6-sol","input":"draw it"}`)
	unchanged, result, err := applyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(
		account,
		"gpt-5.6-sol",
		textBody,
		true,
	)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Equal(t, textBody, unchanged)

	mappedImageBody := []byte(`{"model":"gpt-image-2","input":"draw it","tools":[{"type":"image_generation"}]}`)
	unchanged, result, err = applyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(
		account,
		"gpt-image-2",
		mappedImageBody,
		false,
	)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Equal(t, mappedImageBody, unchanged)
}

func TestOpenAIPromptCacheCreationOptimization_ForwardCodexBridgeKeepsPassiveRequestEligible(t *testing.T) {
	setGinTestMode()
	upstream := &httpUpstreamRecorder{resp: promptCacheBoostJSONResponse("resp_cache_creation_bridge")}
	svc := newOpenAIImageGenerationControlTestService(upstream)
	svc.cfg.Gateway.CodexImageGenerationBridgeEnabled = true
	c, _ := newOpenAIImageGenerationControlTestContext(true, "codex_cli_rs/0.98.0")
	account := newOpenAIImageGenerationControlTestAccount()
	account.Credentials["openai_prompt_cache_creation_optimization_enabled"] = true
	account.Credentials["openai_prompt_cache_creation_optimization_mode"] = OpenAIPromptCacheCreationOptimizationModeSuppress
	body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":"write code","tool_choice":"auto"}`)
	ctx := WithOpenAIExplicitImageGenerationIntent(context.Background(), false)

	result, err := svc.Forward(ctx, c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "explicit", gjson.GetBytes(upstream.lastBody, "prompt_cache_options.mode").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(type=="image_generation")`).Exists(), "bridge tool must still be forwarded")
}

func TestOpenAIPromptCacheCreationOptimization_OAuthEdgeUsesIngressIntentBeforeCachedBodyMutation(t *testing.T) {
	setGinTestMode()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(OpenAIParsedRequestBodyKey, map[string]any{
		"model":  "gpt-5.6-sol",
		"stream": true,
		"input":  "write code",
		"tools": []any{
			map[string]any{"type": "image_generation"},
		},
	})
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	account.Credentials["access_token"] = "oauth-token"
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"write code"}`)

	plan, err := (&OpenAIGatewayService{cfg: promptCacheBoostTestConfig()}).BuildChatGPTOAuthResponsesEdgePlan(
		context.Background(), c, account, body,
	)

	require.NoError(t, err)
	decoded, err := base64.StdEncoding.DecodeString(plan.Plan.BodyRawBase64)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(decoded, `tools.#(type=="image_generation")`).Exists())
	require.Equal(t, "explicit", gjson.GetBytes(decoded, "prompt_cache_options.mode").String())
	require.Equal(t, "24h", gjson.GetBytes(decoded, "prompt_cache_options.ttl").String())
	require.False(t, gjson.GetBytes(decoded, "prompt_cache_retention").Exists())
}

func TestOpenAIPromptCacheCreationOptimization_ExplicitResponsesKeepsKeyAndMarksStablePrefix(t *testing.T) {
	stable := strings.Repeat("stable developer policy ", 260)
	bodyValue := map[string]any{
		"model":                  "gpt-5.6-sol",
		"prompt_cache_key":       "keep-this-key",
		"prompt_cache_retention": "24h",
		"prompt_cache_options":   map[string]any{"mode": "implicit"},
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": stable},
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "dynamic question", "prompt_cache_breakpoint": map[string]any{"mode": "explicit"}},
			}},
		},
	}
	body, err := json.Marshal(bodyValue)
	require.NoError(t, err)
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeReduce)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.True(t, result.BreakpointInserted)
	require.True(t, result.RemovedPromptCacheRetention)
	var got map[string]any
	require.NoError(t, json.Unmarshal(updated, &got))
	require.Equal(t, "keep-this-key", got["prompt_cache_key"])
	require.NotContains(t, got, "prompt_cache_retention")
	require.Equal(t, "explicit", got["prompt_cache_options"].(map[string]any)["mode"])
	require.Equal(t, "24h", got["prompt_cache_options"].(map[string]any)["ttl"])
	input := got["input"].([]any)
	developerParts := input[0].(map[string]any)["content"].([]any)
	require.Equal(t, "explicit", developerParts[0].(map[string]any)["prompt_cache_breakpoint"].(map[string]any)["mode"])
	userParts := input[1].(map[string]any)["content"].([]any)
	require.NotContains(t, userParts[0].(map[string]any), "prompt_cache_breakpoint")
}

func TestOpenAIPromptCacheCreationOptimization_SuppressNeverAddsStablePrefixBreakpoint(t *testing.T) {
	stable := strings.Repeat("stable developer policy ", 260)
	body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_retention":"24h","input":[{"role":"developer","content":"` + stable + `","prompt_cache_breakpoint":{"mode":"explicit"}},{"role":"user","content":"dynamic"}]}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeSuppress)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.False(t, result.BreakpointInserted)
	require.Equal(t, "explicit", gjson.GetBytes(updated, "prompt_cache_options.mode").String())
	require.Equal(t, "24h", gjson.GetBytes(updated, "prompt_cache_options.ttl").String())
	require.False(t, gjson.GetBytes(updated, "prompt_cache_retention").Exists())
	require.False(t, gjson.GetBytes(updated, "input.0.prompt_cache_breakpoint").Exists())
	require.False(t, gjson.GetBytes(updated, "input.1.prompt_cache_breakpoint").Exists())
	require.Equal(t, gjson.String, gjson.GetBytes(updated, "input.0.content").Type)
}

func TestOpenAIPromptCacheCreationOptimization_PreservesBusinessJSONWithBreakpointKey(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","tools":[{"type":"function","function":{"name":"exec","parameters":{"type":"object","properties":{"prompt_cache_breakpoint":{"type":"string"}}}}}],"input":[{"type":"function_call","name":"exec","arguments":{"prompt_cache_breakpoint":"business-value"},"prompt_cache_breakpoint":{"mode":"explicit"}}]}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.False(t, gjson.GetBytes(updated, "input.0.prompt_cache_breakpoint").Exists())
	require.Equal(t, "business-value", gjson.GetBytes(updated, "input.0.arguments.prompt_cache_breakpoint").String())
	require.Equal(t, "string", gjson.GetBytes(updated, "tools.0.function.parameters.properties.prompt_cache_breakpoint.type").String())
}

func TestOpenAIPromptCacheCreationOptimization_PreservesLargeJSONIntegers(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","seed":9007199254740993,"metadata":{"business_id":18446744073709551615},"input":[{"role":"user","content":"hello"}]}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.Contains(t, string(updated), `"seed":9007199254740993`)
	require.Contains(t, string(updated), `"business_id":18446744073709551615`)
}

func TestOpenAIPromptCacheCreationOptimization_ForwardPreservesLargeJSONIntegers(t *testing.T) {
	setGinTestMode()
	body := []byte(`{"model":"gpt-5.6-sol","stream":false,"instructions":"fixed","seed":9007199254740993,"metadata":{"business_id":18446744073709551615},"input":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: promptCacheBoostJSONResponse("resp_cache_creation_large_integer")}
	svc := &OpenAIGatewayService{cfg: promptCacheBoostTestConfig(), httpUpstream: upstream}
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	account.ID = 902
	account.Credentials["api_key"] = "sk-test"
	account.Credentials["base_url"] = "https://api.openai.com/v1"

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, string(upstream.lastBody), `"seed":9007199254740993`)
	require.Contains(t, string(upstream.lastBody), `"business_id":18446744073709551615`)
	require.Equal(t, "explicit", gjson.GetBytes(upstream.lastBody, "prompt_cache_options.mode").String())
}

func TestOpenAIPromptCacheCreationOptimization_RejectsTrailingJSON(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello"} {"unexpected":true}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)

	_, _, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.ErrorContains(t, err, "multiple JSON values")
}

func TestOpenAIPromptCacheCreationOptimization_ReduceWithoutStablePrefixDisablesCacheWrites(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-terra","prompt_cache_retention":"24h","messages":[{"role":"user","content":"dynamic"}]}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeReduce)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-terra", body)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.False(t, result.BreakpointInserted)
	var got map[string]any
	require.NoError(t, json.Unmarshal(updated, &got))
	require.NotContains(t, got, "prompt_cache_retention")
	require.Equal(t, "explicit", got["prompt_cache_options"].(map[string]any)["mode"])
	messages := got["messages"].([]any)
	require.NotContains(t, messages[0].(map[string]any), "prompt_cache_breakpoint")
}

func TestOpenAIPromptCacheCreationOptimization_ShortStableStringKeepsOriginalContentShape(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"developer","content":"short stable policy"},{"role":"user","content":"dynamic"}]}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeReduce)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.Applied)
	require.False(t, result.BreakpointInserted)
	require.Equal(t, "short stable policy", gjson.GetBytes(updated, "input.0.content").String())
	require.Equal(t, gjson.String, gjson.GetBytes(updated, "input.0.content").Type)
}

func TestOpenAIPromptCacheCreationOptimization_ReduceChatMarksOnlyStablePrefix(t *testing.T) {
	stable := strings.Repeat("stable system policy ", 260)
	bodyValue := map[string]any{
		"model": "gpt-5.6-sol",
		"messages": []any{
			map[string]any{"role": "system", "content": stable},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "dynamic", "prompt_cache_breakpoint": map[string]any{"mode": "explicit"}},
			}},
		},
	}
	body, err := json.Marshal(bodyValue)
	require.NoError(t, err)
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeReduce)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.BreakpointInserted)
	require.Equal(t, "text", gjson.GetBytes(updated, "messages.0.content.0.type").String())
	require.Equal(t, "explicit", gjson.GetBytes(updated, "messages.0.content.0.prompt_cache_breakpoint.mode").String())
	require.False(t, gjson.GetBytes(updated, "messages.1.content.0.prompt_cache_breakpoint").Exists())
}

func TestOpenAIPromptCacheCreationOptimization_StopsBeforeUnknownStableContentPart(t *testing.T) {
	stable := strings.Repeat("stable policy ", 400)
	bodyValue := map[string]any{
		"model": "gpt-5.6-sol",
		"input": []any{
			map[string]any{"role": "developer", "content": []any{
				map[string]any{"type": "input_text", "text": stable},
				map[string]any{"type": "unknown_dynamic_part", "value": "must not be cached"},
				map[string]any{"type": "input_text", "text": "after unknown"},
			}},
		},
	}
	body, err := json.Marshal(bodyValue)
	require.NoError(t, err)
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeReduce)

	updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-sol", body)

	require.NoError(t, err)
	require.True(t, result.BreakpointInserted)
	require.Equal(t, "explicit", gjson.GetBytes(updated, "input.0.content.0.prompt_cache_breakpoint.mode").String())
	require.False(t, gjson.GetBytes(updated, "input.0.content.1.prompt_cache_breakpoint").Exists())
	require.False(t, gjson.GetBytes(updated, "input.0.content.2.prompt_cache_breakpoint").Exists())
}

func TestAccountPromptCacheCreationOptimizationScopeAndMode(t *testing.T) {
	oauth := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	require.True(t, oauth.IsOpenAIPromptCacheCreationOptimizationEnabled())
	require.True(t, oauth.IsOpenAIPromptCacheCreationSuppressEnabled())

	apikey := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, "unexpected")
	require.True(t, apikey.IsOpenAIPromptCacheCreationOptimizationEnabled(), "API-key account must not require pool mode")
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeReduce, apikey.OpenAIPromptCacheCreationOptimizationMode())

	legacyReduce := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, openAIPromptCacheCreationOptimizationLegacyReduce)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeReduce, legacyReduce.OpenAIPromptCacheCreationOptimizationMode())
	legacySuppress := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, openAIPromptCacheCreationOptimizationLegacySuppress)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeSuppress, legacySuppress.OpenAIPromptCacheCreationOptimizationMode())

	imagePool := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	imagePool.Credentials["pool_mode"] = true
	imagePool.Credentials["image_pool_mode"] = true
	require.False(t, imagePool.IsOpenAIPromptCacheCreationOptimizationEnabled())

	parentID := int64(1)
	shadow := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	shadow.ParentAccountID = &parentID
	require.False(t, shadow.IsOpenAIPromptCacheCreationOptimizationEnabled())

	otherPlatform := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	otherPlatform.Platform = PlatformAnthropic
	require.False(t, otherPlatform.IsOpenAIPromptCacheCreationOptimizationEnabled())
}

func TestOpenAIPromptCacheCreationOptimization_UnsupportedErrorDetectionIsFieldSpecific(t *testing.T) {
	require.True(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		"Unsupported parameter: 'prompt_cache_options'",
		nil,
	))
	require.True(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusUnprocessableEntity,
		"",
		[]byte(`{"error":{"message":"Unknown field prompt_cache_breakpoint"}}`),
	))
	require.True(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		"Invalid value for prompt_cache_options.mode",
		nil,
	))
	require.True(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		"Additional properties are not allowed: prompt_cache_options",
		nil,
	))
	require.True(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		"",
		[]byte(`{"error":{"message":"Unsupported parameter","param":"prompt_cache_options"}}`),
	))
	require.True(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusUnprocessableEntity,
		"",
		[]byte(`{"detail":[{"type":"extra_forbidden","loc":["body","prompt_cache_options"],"msg":"Extra inputs are not permitted","input":{"mode":"explicit"}}]}`),
	))
	require.True(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		"",
		[]byte(`{"error":{"code":"unknown_parameter","param":"prompt_cache_options"}}`),
	))
	require.False(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		"Unsupported parameter: 'service_tier'",
		nil,
	))
	require.False(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		"",
		[]byte(`{"error":{"message":"Unsupported model name","param":"model"},"request":{"prompt_cache_options":{"mode":"explicit"}}}`),
	), "an echoed request must not turn an unrelated upstream error into a policy compatibility failure")
	require.False(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		`{"error":{"message":"Unsupported model name","param":"model"},"request":{"prompt_cache_options":{"mode":"explicit"}}}`,
		nil,
	), "edge-rs JSON error_message must use the same structured classification")
	require.False(t, isOpenAIPromptCacheCreationOptimizationUnsupportedError(
		http.StatusBadRequest,
		"",
		[]byte(`{"error":{"message":"Invalid schema: property prompt_cache_breakpoint is malformed","param":"tools.0.parameters"}}`),
	), "a business schema property with the same name must not trigger cache-policy fallback")
}

func TestOpenAIPromptCacheCreationOptimization_FallbackAccountDoesNotMutateSharedCredentials(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	account.Credentials["access_token"] = "token"

	fallback := openAIPromptCacheCreationOptimizationFallbackAccount(account)

	require.NotSame(t, account, fallback)
	require.False(t, fallback.IsOpenAIPromptCacheCreationOptimizationEnabled())
	require.Equal(t, "token", fallback.Credentials["access_token"])
	require.True(t, account.IsOpenAIPromptCacheCreationSuppressEnabled())
}

func TestOpenAIPromptCacheCreationOptimization_ForwardRetriesOnceWithoutExplicitFields(t *testing.T) {
	setGinTestMode()
	stable := strings.Repeat("stable developer policy ", 260)
	bodyValue := map[string]any{
		"model":  "gpt-5.6-sol",
		"stream": false,
		"input": []any{
			map[string]any{"role": "developer", "content": stable},
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	body, err := json.Marshal(bodyValue)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		promptCacheBoostUnsupportedResponse("Unsupported parameter: 'prompt_cache_options'"),
		promptCacheBoostJSONResponse("resp_cache_creation_fallback"),
	}}
	svc := &OpenAIGatewayService{cfg: promptCacheBoostTestConfig(), httpUpstream: upstream}
	account := promptCacheBoostTestAccount(901)
	account.Credentials["openai_prompt_cache_creation_optimization_enabled"] = true
	account.Credentials["openai_prompt_cache_creation_optimization_mode"] = OpenAIPromptCacheCreationOptimizationModeReduce

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, "explicit", gjson.GetBytes(upstream.bodies[0], "prompt_cache_options.mode").String())
	require.Equal(t, "24h", gjson.GetBytes(upstream.bodies[0], "prompt_cache_options.ttl").String())
	require.False(t, gjson.GetBytes(upstream.bodies[0], "prompt_cache_retention").Exists())
	require.Equal(t, "explicit", gjson.GetBytes(upstream.bodies[0], "input.0.content.0.prompt_cache_breakpoint.mode").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_options").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "input.0.content.0.prompt_cache_breakpoint").Exists())
	require.Equal(t, "24h", gjson.GetBytes(upstream.bodies[1], "prompt_cache_retention").String())
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeReduce, account.OpenAIPromptCacheCreationOptimizationMode(), "fallback must not mutate the shared account")
}

func TestOpenAIPromptCacheCreationOptimization_SuppressModeStaysOnEdgeRS(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)
	account.Credentials["api_key"] = "sk-test"
	account.Credentials["base_url"] = "https://api.openai.com"
	stable := strings.Repeat("stable system policy ", 260)
	body := []byte(`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"system","content":"` + stable + `"},{"role":"user","content":"hello"}]}`)
	svc := &OpenAIGatewayService{cfg: promptCacheBoostTestConfig()}

	plan, err := svc.BuildRawChatCompletionsEdgePlan(context.Background(), nil, account, body, "")
	require.NoError(t, err)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeSuppress, plan.Plan.PromptCacheCreationOptimizationMode)
	decoded, err := base64.StdEncoding.DecodeString(plan.Plan.BodyRawBase64)
	require.NoError(t, err)
	require.Equal(t, "explicit", gjson.GetBytes(decoded, "prompt_cache_options.mode").String())
	require.False(t, gjson.GetBytes(decoded, "messages.0.content.0.prompt_cache_breakpoint").Exists())
	nonGPT56Body := []byte(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	_, err = svc.BuildRawChatCompletionsEdgePlan(context.Background(), nil, account, nonGPT56Body, "")
	require.NoError(t, err, "mode B must not move unrelated models off edge-rs")

	account.Extra = map[string]any{
		"openai_passthrough":                     true,
		openai_compat.ExtraKeyResponsesSupported: true,
	}
	responsesBody := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"developer","content":"` + stable + `"},{"role":"user","content":"hello"}]}`)
	responsesPlan, err := svc.BuildRawResponsesEdgePlan(context.Background(), nil, account, responsesBody)
	require.NoError(t, err)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeSuppress, responsesPlan.Plan.PromptCacheCreationOptimizationMode)
	responsesDecoded, err := base64.StdEncoding.DecodeString(responsesPlan.Plan.BodyRawBase64)
	require.NoError(t, err)
	require.Equal(t, "explicit", gjson.GetBytes(responsesDecoded, "prompt_cache_options.mode").String())
	require.False(t, gjson.GetBytes(responsesDecoded, "input.0.content.0.prompt_cache_breakpoint").Exists())

	account.Type = AccountTypeOAuth
	account.Credentials["access_token"] = "oauth-token"
	oauthBody := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"developer","content":"` + stable + `"},{"role":"user","content":"hello"}]}`)
	oauthPlan, err := svc.BuildChatGPTOAuthResponsesEdgePlan(context.Background(), nil, account, oauthBody)
	require.NoError(t, err)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeSuppress, oauthPlan.Plan.PromptCacheCreationOptimizationMode)
	oauthDecoded, err := base64.StdEncoding.DecodeString(oauthPlan.Plan.BodyRawBase64)
	require.NoError(t, err)
	require.Equal(t, "explicit", gjson.GetBytes(oauthDecoded, "prompt_cache_options.mode").String())
	require.False(t, gjson.GetBytes(oauthDecoded, "input.0.content.0.prompt_cache_breakpoint").Exists())

	svc.cfg.Gateway.OpenAIWS.Enabled = true
	svc.cfg.Gateway.OpenAIWS.OAuthEnabled = true
	svc.cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	svc.cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	account.Concurrency = 1
	account.Extra = map[string]any{
		"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough,
	}
	wsBody := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":[{"role":"developer","content":"` + stable + `"},{"role":"user","content":"hello"}]}`)
	wsPlan, err := svc.BuildResponsesWSEdgePlan(context.Background(), nil, account, wsBody, "oauth-token")
	require.NoError(t, err)
	require.Equal(t, OpenAIEdgeTransportWSV2, wsPlan.Plan.Transport)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeSuppress, wsPlan.Plan.PromptCacheCreationOptimizationMode)
	require.True(t, wsPlan.Plan.PromptCacheCreationOptimizationApplied)
	require.Equal(t, "gpt-5.6-sol", wsPlan.Plan.PromptCacheCreationOptimizationModel)
	wsDecoded, err := base64.StdEncoding.DecodeString(wsPlan.Plan.BodyRawBase64)
	require.NoError(t, err)
	require.Equal(t, "explicit", gjson.GetBytes(wsDecoded, "prompt_cache_options.mode").String())
	require.False(t, gjson.GetBytes(wsDecoded, "input.0.content.0.prompt_cache_breakpoint").Exists())

	nonTargetWSBody := []byte(`{"type":"response.create","model":"gpt-5.5","input":[{"role":"user","content":"hello"}]}`)
	nonTargetWSPlan, err := svc.BuildResponsesWSEdgePlan(context.Background(), nil, account, nonTargetWSBody, "oauth-token")
	require.NoError(t, err)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeSuppress, nonTargetWSPlan.Plan.PromptCacheCreationOptimizationMode,
		"edge-rs needs the account policy for a later session.update to GPT-5.6")
	require.False(t, nonTargetWSPlan.Plan.PromptCacheCreationOptimizationApplied,
		"carrying the account policy must not claim that a non-target first frame was rewritten")
	require.Equal(t, "gpt-5.5", nonTargetWSPlan.Plan.PromptCacheCreationOptimizationModel)
	nonTargetWSDecoded, err := base64.StdEncoding.DecodeString(nonTargetWSPlan.Plan.BodyRawBase64)
	require.NoError(t, err)
	require.Equal(t, nonTargetWSBody, nonTargetWSDecoded, "the initial non-target frame must remain byte-exact")
}
