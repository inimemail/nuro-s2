package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCNProviderLocalExtraTakesPrecedence(t *testing.T) {
	account := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"account_mode": "payg",
			"api_protocol": "chat_completions",
		},
		Extra: map[string]any{
			"cn_billing_mode": "coding_plan",
			"cn_api_mode":     "anthropic",
		},
	}
	require.Equal(t, CNBillingModeCodingPlan, account.GetCNBillingMode())
	require.Equal(t, AccountModeCoding, account.GetAccountMode())
	require.Equal(t, APIProtocolAnthropic, account.GetAPIProtocol())
	require.Equal(t, DefaultKimiCodingAnthropicBaseURL, account.GetAnthropicProtocolBaseURL())
}

func TestCNProviderLegacyAccountIsExactChatDefault(t *testing.T) {
	for _, tc := range []struct {
		platform string
		baseURL  string
	}{
		{PlatformKimi, DefaultKimiPayGBaseURL},
		{PlatformZhipu, DefaultZhipuPayGBaseURL},
		{PlatformDeepSeek, DefaultDeepSeekChatBaseURL},
	} {
		account := &Account{Platform: tc.platform, Type: AccountTypeAPIKey}
		require.Equal(t, CNBillingModePayG, account.GetCNBillingMode())
		require.Equal(t, APIProtocolChatCompletions, account.GetAPIProtocol())
		require.Equal(t, tc.baseURL, account.GetOpenAIBaseURL())
	}
}

func TestCNProviderAdaptiveUsesLocalProtocolURLs(t *testing.T) {
	account := &Account{
		Platform:    PlatformDeepSeek,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://legacy.example/v1"},
		Extra: map[string]any{
			"cn_api_mode": "adaptive",
			"cn_api_base_urls": map[string]any{
				"chat_completions": "https://chat.example/v1",
				"anthropic":        "https://messages.example",
				"responses":        "https://responses.example",
			},
		},
	}
	require.Equal(t, "https://chat.example/v1", account.GetOpenAIBaseURL())
	require.Equal(t, "https://messages.example", account.GetAnthropicProtocolBaseURL())
	require.Equal(t, "https://responses.example", account.GetCNProtocolBaseURL(APIProtocolResponses))
}

func TestCNProviderResponsesRestrictedToDeepSeek(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu} {
		account := &Account{Platform: platform, Extra: map[string]any{"cn_api_mode": "responses"}}
		require.Equal(t, APIProtocolChatCompletions, account.GetAPIProtocol())
	}
	account := &Account{Platform: PlatformDeepSeek, Extra: map[string]any{"cn_api_mode": "responses"}}
	require.Equal(t, APIProtocolResponses, account.GetAPIProtocol())
	require.Equal(t, DefaultDeepSeekResponsesBaseURL, account.GetCNProtocolBaseURL(APIProtocolResponses))
}

func TestAdaptiveCNProviderResponsesUseNativeAnthropicExceptDeepSeek(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu} {
		account := &Account{Platform: platform, Type: AccountTypeAPIKey, Extra: map[string]any{"cn_api_mode": APIProtocolAdaptive}}
		require.True(t, account.IsAdaptiveAPIProtocol())
		require.NotEqual(t, APIProtocolResponses, account.GetAPIProtocol())
	}
	deepseek := &Account{Platform: PlatformDeepSeek, Type: AccountTypeAPIKey, Extra: map[string]any{"cn_api_mode": APIProtocolAdaptive}}
	require.True(t, deepseek.IsAdaptiveAPIProtocol())
}

func TestCNProviderAnthropicSSEUsageAliases(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(`{"type":"message_start","message":{"usage":{"prompt_tokens":100,"prompt_cache_hit_tokens":60,"prompt_cache_miss_tokens":40}}}`, usage)
	require.Equal(t, 40, usage.InputTokens)
	require.Equal(t, 60, usage.CacheReadInputTokens)

	parseSSEUsagePassthrough(`{"type":"message_delta","usage":{"output_tokens":25,"cached_tokens":60}}`, usage)
	require.Equal(t, 25, usage.OutputTokens)
	require.Equal(t, 60, usage.CacheReadInputTokens)
}

func TestDeepSeekResponsesNormalizationIsProtocolScoped(t *testing.T) {
	body := []byte(`{"model":"deepseek-chat","store":true,"previous_response_id":"resp_1","input":"hi"}`)
	responses := &Account{Platform: PlatformDeepSeek, Extra: map[string]any{"cn_api_mode": APIProtocolResponses}}
	normalized := normalizeDeepSeekResponsesRequestBody(responses, body)
	require.False(t, gjson.GetBytes(normalized, "store").Bool())
	require.False(t, gjson.GetBytes(normalized, "previous_response_id").Exists())

	chat := &Account{Platform: PlatformDeepSeek, Extra: map[string]any{"cn_api_mode": APIProtocolChatCompletions}}
	require.Equal(t, body, normalizeDeepSeekResponsesRequestBody(chat, body))
}

func TestCNProviderQuotaParsers(t *testing.T) {
	kimi := parseKimiUsageTiers([]byte(`{
		"limits":[{"detail":{"limit":100,"remaining":25,"resetTime":1787558400000}}],
		"usage":{"limit":"1000","remaining":"700","resetTime":"1788163200000"}
	}`))
	require.Len(t, kimi, 2)
	require.Equal(t, "5h", kimi[0].Window)
	require.InDelta(t, 75, kimi[0].UsedPercent, 1e-9)
	require.Equal(t, "weekly", kimi[1].Window)
	require.InDelta(t, 30, kimi[1].UsedPercent, 1e-9)

	zhipu := parseZhipuTokenTiers(gjson.Parse(`{"limits":[
		{"type":"CREDIT_LIMIT","percentage":99,"unit":3},
		{"type":"TOKENS_LIMIT","percentage":22,"unit":6,"nextResetTime":1788163200000},
		{"type":"TOKENS_LIMIT","percentage":11,"unit":3,"nextResetTime":1787558400000}
	]}`))
	require.Len(t, zhipu, 2)
	require.Equal(t, "5h", zhipu[0].Window)
	require.InDelta(t, 11, zhipu[0].UsedPercent, 1e-9)
	require.Equal(t, "weekly", zhipu[1].Window)
	require.InDelta(t, 22, zhipu[1].UsedPercent, 1e-9)
}

func TestParseKimiBalanceResponseRejectsBusinessErrorsAndMissingValues(t *testing.T) {
	_, err := parseKimiBalanceResponse([]byte(`{"code":401,"data":{"available_balance":0}}`))
	require.Error(t, err)
	_, err = parseKimiBalanceResponse([]byte(`{"code":0,"data":{}}`))
	require.Error(t, err)
	balance, err := parseKimiBalanceResponse([]byte(`{"code":0,"data":{"available_balance":12.5}}`))
	require.NoError(t, err)
	require.Equal(t, 12.5, balance)
}

func TestCNQuotaParsersRejectIncompleteWindows(t *testing.T) {
	t.Parallel()
	require.Empty(t, parseKimiUsageTiers([]byte(`{"limits":[{"detail":{"limit":100}}],"usage":{"limit":100}}`)))
	require.Empty(t, parseZhipuTokenTiers(gjson.Parse(`{"limits":[{"type":"TOKENS_LIMIT"}]}`)))
}
