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

func TestMiniMaxSupportsAllDomesticProtocolsAndDefaults(t *testing.T) {
	account := &Account{Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": DefaultMiniMaxCNBaseURL}, Extra: map[string]any{"cn_api_mode": APIProtocolResponses}}
	require.True(t, account.IsCNProvider())
	require.Equal(t, APIProtocolResponses, account.GetAPIProtocol())
	require.Equal(t, DefaultMiniMaxCNBaseURL, account.GetCNProtocolBaseURL(APIProtocolResponses))
	require.Equal(t, DefaultMiniMaxCNAnthropicBaseURL, account.GetCNProtocolBaseURL(APIProtocolAnthropic))
}

func TestCNAdaptiveDerivationFollowsPrimaryUnlessExplicitlyOverridden(t *testing.T) {
	extra, _ := normalizeCNProviderStoredConfig(PlatformMiniMax, map[string]any{
		cnAPIProtocolExtraKey: APIProtocolAdaptive,
		cnAPIBaseURLsExtraKey: map[string]any{
			APIProtocolChatCompletions: "https://old.example/v1",
			APIProtocolAnthropic:       "https://old.example/anthropic",
			APIProtocolResponses:       "https://old.example/v1",
		},
	}, map[string]any{"base_url": "https://new.example/v1"})
	urls := cnStringMap(extra[cnAPIBaseURLsExtraKey])
	require.Equal(t, "https://new.example/v1", urls[APIProtocolChatCompletions])
	require.Equal(t, "https://new.example/anthropic", urls[APIProtocolAnthropic])
	require.Equal(t, "https://new.example/v1", urls[APIProtocolResponses])

	extra[cnAPIBaseURLOverridesExtraKey] = map[string]any{APIProtocolAnthropic: true}
	extra[cnAPIBaseURLsExtraKey] = map[string]any{APIProtocolAnthropic: "https://manual.example/messages"}
	extra, _ = normalizeCNProviderStoredConfig(PlatformMiniMax, extra, map[string]any{"base_url": "https://newer.example/v1"})
	urls = cnStringMap(extra[cnAPIBaseURLsExtraKey])
	require.Equal(t, "https://manual.example/messages", urls[APIProtocolAnthropic])
	require.Equal(t, "https://newer.example/v1", urls[APIProtocolChatCompletions])
}

func TestNormalizeBulkUpdateForNonCNAccountDropsDomesticEndpointConfig(t *testing.T) {
	updates := normalizeBulkUpdateForAccount(&Account{Platform: PlatformOpenAI}, AccountBulkUpdate{
		Extra: map[string]any{
			cnAPIProtocolExtraKey:         APIProtocolAdaptive,
			cnAPIBaseURLsExtraKey:         map[string]any{APIProtocolChatCompletions: "https://example.test/v1"},
			cnAPIBaseURLOverridesExtraKey: map[string]any{APIProtocolAnthropic: true},
		},
		ExtraRemoveKeys: []string{cnAPIProtocolExtraKey, cnAPIBaseURLsExtraKey, cnAPIBaseURLOverridesExtraKey},
	})
	require.NotContains(t, updates.Extra, cnAPIProtocolExtraKey)
	require.NotContains(t, updates.Extra, cnAPIBaseURLsExtraKey)
	require.NotContains(t, updates.Extra, cnAPIBaseURLOverridesExtraKey)
	require.NotContains(t, updates.ExtraRemoveKeys, cnAPIProtocolExtraKey)
	require.NotContains(t, updates.ExtraRemoveKeys, cnAPIBaseURLsExtraKey)
	require.NotContains(t, updates.ExtraRemoveKeys, cnAPIBaseURLOverridesExtraKey)
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
	minimax := &Account{Platform: PlatformMiniMax, Type: AccountTypeAPIKey, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolResponses}}
	require.True(t, shouldForwardAPIKeyChatViaResponses(minimax))
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

	minimax := parseMiniMaxUsageTiers([]byte(`{
		"current_subscribe_title":"MiniMax Coding Plan",
		"model_remains":[
			{"model_name":"other","current_interval_remaining_percent":99},
			{"model_name":"general","current_interval_remaining_percent":25,"current_weekly_status":1,"current_weekly_remaining_percent":60}
		]
	}`))
	require.Len(t, minimax, 2)
	require.Equal(t, "5h", minimax[0].Window)
	require.InDelta(t, 75, minimax[0].UsedPercent, 1e-9)
	require.Equal(t, "weekly", minimax[1].Window)
	require.InDelta(t, 40, minimax[1].UsedPercent, 1e-9)
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
