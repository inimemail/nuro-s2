package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestApplyHeaderOverridesAppliesAuthoritativeZhipuTeamHeaders(t *testing.T) {
	account := &Account{
		Platform: PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"account_mode":       AccountModeCoding,
			"zhipu_organization": " org-team ",
			"zhipu_project":      " proj-team ",
		},
	}
	header := http.Header{
		"Bigmodel-Organization": []string{"client-org"},
		"Bigmodel-Project":      []string{"client-project"},
	}

	account.ApplyHeaderOverrides(header)

	require.Equal(t, "org-team", header.Get("bigmodel-organization"))
	require.Equal(t, "proj-team", header.Get("bigmodel-project"))
}

func TestApplyHeaderOverridesClearsZhipuTeamHeadersWithoutOrganization(t *testing.T) {
	account := &Account{
		Platform: PlatformZhipu,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"account_mode":  AccountModeCoding,
			"zhipu_project": "stale-project",
		},
	}
	header := http.Header{
		"Bigmodel-Organization": []string{"client-org"},
		"Bigmodel-Project":      []string{"client-project"},
	}

	account.ApplyHeaderOverrides(header)

	require.Empty(t, header.Get("bigmodel-organization"))
	require.Empty(t, header.Get("bigmodel-project"))
}

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

func TestCNProviderAdaptiveUsesConfiguredProtocolURLs(t *testing.T) {
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

func TestCNProviderAdaptiveLegacyBaseURLRemainsChatEndpoint(t *testing.T) {
	account := &Account{
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://legacy-gateway.example/v1"},
		Extra:       map[string]any{cnAPIProtocolExtraKey: APIProtocolAdaptive},
	}
	require.Equal(t, "https://legacy-gateway.example/v1", account.GetOpenAIBaseURL())
}

func TestCNProviderAnthropicDoesNotReuseAnthropicBaseURLForOpenAIFormat(t *testing.T) {
	account := &Account{
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://legacy-gateway.example/anthropic"},
		Extra:       map[string]any{cnAPIProtocolExtraKey: APIProtocolAnthropic},
	}
	require.Equal(t, DefaultKimiPayGBaseURL, account.GetOpenAIFormatBaseURL())
}

func TestCNProviderResponsesSupportedByKimiAndDeepSeek(t *testing.T) {
	for _, platform := range []string{PlatformZhipu} {
		account := &Account{Platform: platform, Extra: map[string]any{"cn_api_mode": "responses"}}
		require.Equal(t, APIProtocolChatCompletions, account.GetAPIProtocol())
	}
	kimi := &Account{Platform: PlatformKimi, Extra: map[string]any{"cn_api_mode": "responses"}}
	require.Equal(t, APIProtocolResponses, kimi.GetAPIProtocol())
	account := &Account{Platform: PlatformDeepSeek, Extra: map[string]any{"cn_api_mode": "responses"}}
	require.Equal(t, APIProtocolResponses, account.GetAPIProtocol())
	require.Equal(t, DefaultDeepSeekResponsesBaseURL, account.GetCNProtocolBaseURL(APIProtocolResponses))
}

func TestDeepSeekResponsesUsesConfiguredResponsesBaseURL(t *testing.T) {
	account := &Account{
		Platform: PlatformDeepSeek,
		Extra: map[string]any{
			cnAPIProtocolExtraKey: APIProtocolResponses,
			cnAPIBaseURLsExtraKey: map[string]any{APIProtocolResponses: "https://responses.proxy/v1"},
		},
	}
	require.Equal(t, "https://responses.proxy/v1", account.GetCNProtocolBaseURL(APIProtocolResponses))
	require.Empty(t, account.GetCNProtocolBaseURL(APIProtocolAnthropic))
}

func TestCNProtocolControlsChatCompletionsResponsesBridge(t *testing.T) {
	deepseek := &Account{Platform: PlatformDeepSeek, Type: AccountTypeAPIKey, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolResponses}}
	require.True(t, shouldForwardAPIKeyChatViaResponses(deepseek))

	kimi := &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolChatCompletions}}
	require.False(t, shouldForwardAPIKeyChatViaResponses(kimi))
}

func TestNormalizeCNProviderStoredConfigPreservesOnlySupportedResponses(t *testing.T) {
	for _, platform := range []string{PlatformZhipu} {
		extra, credentials := normalizeCNProviderStoredConfig(platform,
			map[string]any{cnAPIProtocolExtraKey: APIProtocolResponses, cnAPIBaseURLsExtraKey: map[string]any{"responses": "https://old.example"}},
			map[string]any{"api_key": "secret", "api_protocol": APIProtocolAnthropic, "api_base_urls": map[string]any{"anthropic": "https://old.example"}},
		)
		require.Equal(t, APIProtocolChatCompletions, extra[cnAPIProtocolExtraKey])
		require.Contains(t, extra, cnAPIBaseURLsExtraKey)
		require.Equal(t, "secret", credentials["api_key"])
		require.NotContains(t, credentials, "api_protocol")
		require.NotContains(t, credentials, "api_base_urls")
	}

	for _, platform := range []string{PlatformKimi, PlatformDeepSeek} {
		extra, credentials := normalizeCNProviderStoredConfig(platform, nil, map[string]any{
			"api_key":      "secret",
			"api_protocol": APIProtocolResponses,
		})
		require.Equal(t, APIProtocolResponses, extra[cnAPIProtocolExtraKey])
		require.Equal(t, "secret", credentials["api_key"])
		require.NotContains(t, credentials, "api_protocol")
	}
}

func TestLegacyAdaptiveCNProviderValuesRemainSupported(t *testing.T) {
	for _, platform := range []string{PlatformKimi, PlatformZhipu} {
		account := &Account{Platform: platform, Type: AccountTypeAPIKey, Extra: map[string]any{"cn_api_mode": APIProtocolAdaptive}}
		require.True(t, account.IsAdaptiveAPIProtocol())
		require.Equal(t, APIProtocolAdaptive, account.GetAPIProtocol())
		require.NotEqual(t, APIProtocolResponses, account.GetAPIProtocol())
	}
	deepseek := &Account{Platform: PlatformDeepSeek, Type: AccountTypeAPIKey, Extra: map[string]any{"cn_api_mode": APIProtocolAdaptive}}
	require.True(t, deepseek.IsAdaptiveAPIProtocol())
	require.Equal(t, APIProtocolAdaptive, deepseek.GetAPIProtocol())
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
