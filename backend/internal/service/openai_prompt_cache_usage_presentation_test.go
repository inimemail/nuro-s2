package service

import (
	"context"
	"math"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIDownstreamCacheUsageMode_IsAccountScopedAndExplicit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account *Account
		model   string
		want    string
	}{
		{"disabled C", promptCacheCreationOptimizationAccount(AccountTypeAPIKey, false, OpenAIPromptCacheCreationOptimizationModeFree), "gpt-5.6-terra", ""},
		{"A", promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeReduce), "gpt-5.6-terra", ""},
		{"B", promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress), "gpt-5.6-terra", ""},
		{"C", promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeFree), "gpt-5.6-terra", OpenAIPromptCacheCreationOptimizationModeFree},
		{"D", promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeInput125), "gpt-5.6-sol", OpenAIPromptCacheCreationOptimizationModeInput125},
		{"other model", promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeFree), "gpt-5.5", ""},
		{"image pool", func() *Account {
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
			account.Credentials["pool_mode"] = true
			account.Credentials["image_pool_mode"] = true
			return account
		}(), "gpt-5.6-terra", ""},
		{"shadow account", func() *Account {
			account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeFree)
			parentID := int64(1)
			account.ParentAccountID = &parentID
			return account
		}(), "gpt-5.6-terra", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIDownstreamCacheUsageMode(tc.account, tc.model))
		})
	}
}

func TestOpenAIDownstreamCacheUsageModeForContext_ExplicitImageRequestIsNoOp(t *testing.T) {
	account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeInput125,
		openAIDownstreamCacheUsageModeForContext(context.Background(), account, "gpt-5.6-terra"))
	require.Empty(t, openAIDownstreamCacheUsageModeForContext(
		WithOpenAIImageGenerationIntent(context.Background()), account, "gpt-5.6-terra"),
	)
}

func TestShouldNormalizeOpenAIStreamUsageForDownstream_TerminalOnly(t *testing.T) {
	preToken := []byte(`{"type":"response.created","response":{"input":"literal cache_creation_input_tokens text"}}`)
	require.False(t, shouldNormalizeOpenAIStreamUsageForDownstream(preToken, "response.created"))
	require.False(t, shouldNormalizeOpenAIStreamUsageForDownstream([]byte(`{"type":"response.output_text.delta","delta":"ok"}`), "response.output_text.delta"))
	require.True(t, shouldNormalizeOpenAIStreamUsageForDownstream(
		[]byte(`{"type":"response.completed","response":{"usage":{"cache_creation_input_tokens":1}}}`),
		"response.completed",
	))
	require.True(t, shouldNormalizeOpenAIStreamUsageForDownstream(
		[]byte(`{"usage":{"cache_write_tokens":1}}`),
		"",
	))
}

func TestNormalizeOpenAIWSDownstreamCacheUsage_TerminalOnlyAndByteExactNoOp(t *testing.T) {
	terminal := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_write_tokens":512}}}}`)
	internal, ok := extractOpenAIUsageFromJSONBytes(terminal)
	require.True(t, ok)

	for _, tc := range []struct {
		mode         string
		wantInput    int
		wantCreation int
	}{
		{OpenAIPromptCacheCreationOptimizationModeSuppress, 5724, 512},
		{OpenAIPromptCacheCreationOptimizationModeFree, 5212, 0},
		{OpenAIPromptCacheCreationOptimizationModeInput125, 5858, 0},
	} {
		got := normalizeOpenAIWSDownstreamCacheUsage(terminal, "response.completed", tc.mode)
		downstream, parsed := extractOpenAIUsageFromJSONBytes(got)
		require.True(t, parsed)
		require.Equal(t, tc.wantInput, downstream.InputTokens)
		require.Equal(t, tc.wantCreation, downstream.CacheCreationInputTokens)
		if tc.mode == OpenAIPromptCacheCreationOptimizationModeSuppress {
			require.Equal(t, terminal, got)
		}
	}
	require.Equal(t, 5724, internal.InputTokens)
	require.Equal(t, 512, internal.CacheCreationInputTokens)

	preToken := []byte(`{"type":"response.output_text.delta","delta":"literal cache_creation_input_tokens"}`)
	require.Equal(t, preToken, normalizeOpenAIWSDownstreamCacheUsage(preToken, "response.output_text.delta", OpenAIPromptCacheCreationOptimizationModeInput125))
}

func TestOpenAIDownstreamCacheUsageMode_CompatibilityFallbackPreservesOnlySelectedCD(t *testing.T) {
	selected := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeFree)
	fallback := openAIPromptCacheCreationOptimizationFallbackAccount(selected)
	require.False(t, fallback.IsOpenAIPromptCacheCreationOptimizationEnabled(), "fallback must disable unsupported request fields")
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeFree, openAIDownstreamCacheUsageMode(fallback, "gpt-5.6-terra"))
	require.Empty(t, openAIDownstreamCacheUsageMode(fallback, "gpt-5.5"))
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeFree, openAIDownstreamCacheUsageMode(selected, "gpt-5.6-terra"))

	for _, mode := range []string{OpenAIPromptCacheCreationOptimizationModeReduce, OpenAIPromptCacheCreationOptimizationModeSuppress} {
		other := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, mode)
		require.Empty(t, openAIDownstreamCacheUsageMode(openAIPromptCacheCreationOptimizationFallbackAccount(other), "gpt-5.6-terra"))
	}
}

func TestOpenAIPromptCacheCreationOptimization_CDUseBRequestPolicy(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-terra","prompt_cache_retention":"24h","prompt_cache_options":{"mode":"implicit","ttl":"24h"},"input":[{"role":"developer","content":"stable","prompt_cache_breakpoint":{"mode":"explicit"}}]}`)
	for _, mode := range []string{OpenAIPromptCacheCreationOptimizationModeFree, OpenAIPromptCacheCreationOptimizationModeInput125} {
		account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, mode)
		updated, result, err := applyOpenAIPromptCacheCreationOptimizationBody(account, "gpt-5.6-terra", body)
		require.NoError(t, err)
		require.True(t, result.Applied)
		require.Equal(t, "explicit", gjson.GetBytes(updated, "prompt_cache_options.mode").String())
		require.Equal(t, "24h", gjson.GetBytes(updated, "prompt_cache_options.ttl").String())
		require.False(t, gjson.GetBytes(updated, "prompt_cache_retention").Exists())
		require.False(t, gjson.GetBytes(updated, "input.0.prompt_cache_breakpoint").Exists())
	}
}

func TestOpenAIEdgePlan_CarriesDownstreamModeIndependentlyFromRequestFallback(t *testing.T) {
	for _, mode := range []string{OpenAIPromptCacheCreationOptimizationModeFree, OpenAIPromptCacheCreationOptimizationModeInput125} {
		account := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, mode)
		account.Credentials["api_key"] = "sk-test"
		account.Credentials["base_url"] = "https://api.openai.com"
		svc := &OpenAIGatewayService{cfg: promptCacheBoostTestConfig()}
		body := []byte(`{"model":"gpt-5.6-terra","stream":true,"messages":[{"role":"user","content":"hello"}]}`)

		plan, err := svc.BuildRawChatCompletionsEdgePlan(context.Background(), nil, account, body, "")
		require.NoError(t, err)
		require.Equal(t, mode, plan.Plan.DownstreamCacheUsageMode)
		require.Equal(t, "gpt-5.6-terra", plan.Plan.DownstreamCacheUsageModel)

		fallback := openAIPromptCacheCreationOptimizationFallbackAccount(account)
		fallbackPlan, err := svc.BuildRawChatCompletionsEdgePlan(context.Background(), nil, fallback, body, "")
		require.NoError(t, err)
		require.Empty(t, fallbackPlan.Plan.PromptCacheCreationOptimizationMode)
		require.False(t, fallbackPlan.Plan.PromptCacheCreationOptimizationApplied)
		require.Equal(t, mode, fallbackPlan.Plan.DownstreamCacheUsageMode)
		require.Equal(t, "gpt-5.6-terra", fallbackPlan.Plan.DownstreamCacheUsageModel)
	}
}

func TestNormalizeOpenAIDownstreamUsageJSON_ClosedModesAreByteExactNoOp(t *testing.T) {
	body := []byte(" {\n  \"usage\": {\"input_tokens\":5724,\"cache_creation_input_tokens\":512}\n}")
	for _, mode := range []string{"", OpenAIPromptCacheCreationOptimizationModeReduce, OpenAIPromptCacheCreationOptimizationModeSuppress, "unknown"} {
		got, changed := normalizeOpenAIDownstreamUsageJSON(body, mode)
		require.False(t, changed)
		require.Equal(t, body, got)
	}
}

func TestNormalizeOpenAIDownstreamUsageForRequest_DoesNotLeakAcrossAccounts(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_write_tokens":512}}}`)
	enabledD := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	disabledD := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, false, OpenAIPromptCacheCreationOptimizationModeInput125)
	enabledB := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress)

	updated, changed := normalizeOpenAIDownstreamUsageForRequest(body, context.Background(), enabledD, "gpt-5.6-terra")
	require.True(t, changed)
	require.Equal(t, int64(5858), gjson.GetBytes(updated, "usage.input_tokens").Int())

	for _, account := range []*Account{disabledD, enabledB} {
		got, accountChanged := normalizeOpenAIDownstreamUsageForRequest(body, context.Background(), account, "gpt-5.6-terra")
		require.False(t, accountChanged)
		require.Equal(t, body, got)
	}

	got, changed := normalizeOpenAIDownstreamUsageForRequest(body, context.Background(), enabledD, "gpt-5.5")
	require.False(t, changed)
	require.Equal(t, body, got)
}

func TestOpenAIEdgePlan_DownstreamModeIsOmittedForUnselectedAccounts(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-terra","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	svc := &OpenAIGatewayService{cfg: promptCacheBoostTestConfig()}
	for _, account := range []*Account{
		promptCacheCreationOptimizationAccount(AccountTypeAPIKey, false, OpenAIPromptCacheCreationOptimizationModeInput125),
		promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeReduce),
		promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeSuppress),
	} {
		account.Credentials["api_key"] = "sk-test"
		account.Credentials["base_url"] = "https://api.openai.com"
		plan, err := svc.BuildRawChatCompletionsEdgePlan(context.Background(), nil, account, body, "")
		require.NoError(t, err)
		require.Empty(t, plan.Plan.DownstreamCacheUsageMode)
		require.Empty(t, plan.Plan.DownstreamCacheUsageModel)
	}

	selected := promptCacheCreationOptimizationAccount(AccountTypeAPIKey, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	selected.Credentials["api_key"] = "sk-test"
	selected.Credentials["base_url"] = "https://api.openai.com"
	imageCtx := WithOpenAIImageGenerationIntent(context.Background())
	plan, err := svc.BuildRawChatCompletionsEdgePlan(imageCtx, nil, selected, body, "")
	require.NoError(t, err)
	require.Empty(t, plan.Plan.DownstreamCacheUsageMode)
	require.Empty(t, plan.Plan.DownstreamCacheUsageModel)
}

func TestOpenAIWSEdgePlan_CarriesSelectedModeAcrossInitiallyIneligibleModel(t *testing.T) {
	body := []byte(`{"type":"response.create","model":"gpt-5.5","input":"hello"}`)
	account := promptCacheCreationOptimizationAccount(AccountTypeOAuth, true, OpenAIPromptCacheCreationOptimizationModeInput125)
	account.Credentials["access_token"] = "test-token"
	account.Concurrency = 1
	account.Extra = map[string]any{
		"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough,
	}
	svc := &OpenAIGatewayService{cfg: promptCacheBoostTestConfig()}
	svc.cfg.Gateway.OpenAIWS.Enabled = true
	svc.cfg.Gateway.OpenAIWS.OAuthEnabled = true
	svc.cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	svc.cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool

	plan, err := svc.BuildResponsesWSEdgePlan(context.Background(), nil, account, body, "test-token")
	require.NoError(t, err)
	require.Equal(t, OpenAIPromptCacheCreationOptimizationModeInput125, plan.Plan.DownstreamCacheUsageMode)
	require.Empty(t, plan.Plan.DownstreamCacheUsageModel,
		"the first non-GPT-5.6 turn stays ineligible while edge-rs retains the account mode for later turns")
}

func TestNormalizeOpenAIDownstreamUsageJSON_CFree(t *testing.T) {
	body := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_write_tokens":512},"cache_creation_input_tokens":512}}}`)
	got, changed := normalizeOpenAIDownstreamUsageJSON(body, OpenAIPromptCacheCreationOptimizationModeFree)
	require.True(t, changed)
	require.Equal(t, int64(5212), gjson.GetBytes(got, "response.usage.input_tokens").Int())
	require.Equal(t, int64(5232), gjson.GetBytes(got, "response.usage.total_tokens").Int())
	require.Equal(t, int64(4736), gjson.GetBytes(got, "response.usage.input_tokens_details.cached_tokens").Int())
	require.Zero(t, gjson.GetBytes(got, "response.usage.input_tokens_details.cache_write_tokens").Int())
	require.Zero(t, gjson.GetBytes(got, "response.usage.cache_creation_input_tokens").Int())
	parsed, ok := extractOpenAIUsageFromJSONBytes(got)
	require.True(t, ok)
	require.Equal(t, 476, parsed.InputTokens-parsed.CacheReadInputTokens-parsed.CacheCreationInputTokens)
}

func TestNormalizeOpenAIDownstreamUsageJSON_CDZeroEveryCreationAlias(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":5724,"prompt_tokens":5724,"output_tokens":20,"completion_tokens":20,"total_tokens":5744,"cache_write_tokens":512,"cache_creation_input_tokens":512,"cache_write_input_tokens":512,"cache_creation_tokens":512,"input_tokens_details":{"cached_tokens":4736,"cache_creation_input_tokens":512,"cache_write_input_tokens":512,"cache_write_tokens":512,"cache_creation_tokens":512},"prompt_tokens_details":{"cached_tokens":4736,"cache_creation_input_tokens":512,"cache_write_input_tokens":512,"cache_write_tokens":512,"cache_creation_tokens":512}}}`)
	paths := []string{
		"usage.cache_write_tokens",
		"usage.cache_creation_input_tokens",
		"usage.cache_write_input_tokens",
		"usage.cache_creation_tokens",
		"usage.input_tokens_details.cache_creation_input_tokens",
		"usage.input_tokens_details.cache_write_input_tokens",
		"usage.input_tokens_details.cache_write_tokens",
		"usage.input_tokens_details.cache_creation_tokens",
		"usage.prompt_tokens_details.cache_creation_input_tokens",
		"usage.prompt_tokens_details.cache_write_input_tokens",
		"usage.prompt_tokens_details.cache_write_tokens",
		"usage.prompt_tokens_details.cache_creation_tokens",
	}
	for _, mode := range []string{OpenAIPromptCacheCreationOptimizationModeFree, OpenAIPromptCacheCreationOptimizationModeInput125} {
		got, changed := normalizeOpenAIDownstreamUsageJSON(body, mode)
		require.True(t, changed)
		for _, path := range paths {
			require.True(t, gjson.GetBytes(got, path).Exists(), "mode=%s path=%s", mode, path)
			require.Zero(t, gjson.GetBytes(got, path).Int(), "mode=%s path=%s", mode, path)
		}
	}
}

func TestNormalizeOpenAIDownstreamUsageJSON_DInput125(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":5724,"completion_tokens":20,"total_tokens":5744,"prompt_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}`)
	got, changed := normalizeOpenAIDownstreamUsageJSON(body, OpenAIPromptCacheCreationOptimizationModeInput125)
	require.True(t, changed)
	require.Equal(t, int64(5858), gjson.GetBytes(got, "usage.prompt_tokens").Int())
	require.Equal(t, int64(5880), gjson.GetBytes(got, "usage.total_tokens").Int())
	require.Equal(t, int64(4756), gjson.GetBytes(got, "usage.prompt_tokens_details.cached_tokens").Int())
	require.Equal(t, int64(22), gjson.GetBytes(got, "usage.completion_tokens").Int())
	require.Zero(t, gjson.GetBytes(got, "usage.prompt_tokens_details.cache_creation_tokens").Int())
	parsed, ok := extractOpenAIUsageFromJSONBytes(got)
	require.True(t, ok)
	require.Equal(t, 1102, parsed.InputTokens-parsed.CacheReadInputTokens-parsed.CacheCreationInputTokens)
	require.Equal(t, 4756, parsed.CacheReadInputTokens)
	require.Equal(t, 22, parsed.OutputTokens)
}

func TestNormalizeOpenAIDownstreamUsageJSON_DAddsCanonicalCacheReadWhenUpstreamOmitsIt(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":988,"output_tokens":20,"total_tokens":1008,"cache_read_tokens":0,"input_tokens_details":{"cache_creation_tokens":512}}}`)
	got, changed := normalizeOpenAIDownstreamUsageJSON(body, OpenAIPromptCacheCreationOptimizationModeInput125)
	require.True(t, changed)
	require.Equal(t, int64(1122), gjson.GetBytes(got, "usage.input_tokens").Int())
	require.Equal(t, int64(1144), gjson.GetBytes(got, "usage.total_tokens").Int())
	require.Equal(t, int64(20), gjson.GetBytes(got, "usage.input_tokens_details.cached_tokens").Int())
	require.Equal(t, int64(20), gjson.GetBytes(got, "usage.cache_read_tokens").Int())
	require.Equal(t, int64(22), gjson.GetBytes(got, "usage.output_tokens").Int())
	require.Zero(t, gjson.GetBytes(got, "usage.input_tokens_details.cache_creation_tokens").Int())

	parsed, ok := extractOpenAIUsageFromJSONBytes(got)
	require.True(t, ok)
	require.Equal(t, 1102, parsed.InputTokens-parsed.CacheReadInputTokens-parsed.CacheCreationInputTokens)
	require.Equal(t, 20, parsed.CacheReadInputTokens)
	require.Equal(t, 22, parsed.OutputTokens)
}

func TestNormalizeOpenAIDownstreamUsageJSON_DUsesValidCompletionAliasWhenOutputIsInvalid(t *testing.T) {
	for _, invalidOutput := range []string{"null", "-5"} {
		body := []byte(`{"usage":{"prompt_tokens":5724,"output_tokens":` + invalidOutput + `,"completion_tokens":20,"total_tokens":5744,"prompt_tokens_details":{"cached_tokens":4736,"cache_creation_tokens":512}}}`)
		got, changed := normalizeOpenAIDownstreamUsageJSON(body, OpenAIPromptCacheCreationOptimizationModeInput125)
		require.True(t, changed)
		require.Equal(t, int64(22), gjson.GetBytes(got, "usage.output_tokens").Int())
		require.Equal(t, int64(22), gjson.GetBytes(got, "usage.completion_tokens").Int())
	}
}

func TestNormalizeOpenAIDownstreamUsageJSON_NegativeInputCannotOverflow(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":-9223372036854775808,"output_tokens":1,"total_tokens":0,"input_tokens_details":{"cached_tokens":10,"cache_creation_tokens":1}}}`)
	got, changed := normalizeOpenAIDownstreamUsageJSON(body, OpenAIPromptCacheCreationOptimizationModeFree)
	require.True(t, changed)
	require.Equal(t, int64(10), gjson.GetBytes(got, "usage.input_tokens").Int())
	require.Equal(t, int64(11), gjson.GetBytes(got, "usage.total_tokens").Int())
	require.Zero(t, gjson.GetBytes(got, "usage.input_tokens_details.cache_creation_tokens").Int())
}

func TestAllocateOpenAIInput125DisplayUsage_SaturatesWithoutOverflow(t *testing.T) {
	allocation := allocateOpenAIInput125DisplayUsage(0, 0, math.MaxInt64, math.MaxInt64, true)
	require.Equal(t, int64(math.MaxInt64), allocation.ordinary)
	require.Positive(t, allocation.cacheRead)
	require.Zero(t, allocation.creation)
	require.Equal(t, int64(math.MaxInt64), allocation.output)
}

func TestAllocateOpenAIInput125DisplayUsage_PreservesLongContextBoundaryAndCost(t *testing.T) {
	billing := NewBillingService(&config.Config{}, nil)
	for _, tc := range []struct {
		name     string
		ordinary int64
		read     int64
		creation int64
		output   int64
		wantLong bool
	}{
		{"at_threshold", 271_000, 488, 512, 20, false},
		{"above_threshold", 271_001, 488, 512, 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allocation := allocateOpenAIInput125DisplayUsage(tc.ordinary, tc.read, tc.creation, tc.output, true)
			original, err := billing.CalculateCost("gpt-5.6-terra", UsageTokens{
				InputTokens: int(tc.ordinary), CacheReadTokens: int(tc.read),
				CacheCreationTokens: int(tc.creation), OutputTokens: int(tc.output),
			}, 1)
			require.NoError(t, err)
			displayed, err := billing.CalculateCost("gpt-5.6-terra", UsageTokens{
				InputTokens: int(allocation.ordinary), CacheReadTokens: int(allocation.cacheRead),
				CacheCreationTokens: int(allocation.creation), OutputTokens: int(allocation.output),
			}, 1)
			require.NoError(t, err)
			require.Equal(t, tc.wantLong, original.LongContextBillingApplied)
			require.Equal(t, tc.wantLong, displayed.LongContextBillingApplied)
			require.InDelta(t, original.TotalCost, displayed.TotalCost, 1e-12)
		})
	}
}

func TestNormalizeOpenAIDownstreamUsageJSON_MatchesDownstreamSourceBillingBuckets(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":5724,"output_tokens":20,"total_tokens":5744,"input_tokens_details":{"cached_tokens":4736,"cache_write_tokens":512}}}`)

	freeBody, changed := normalizeOpenAIDownstreamUsageJSON(body, OpenAIPromptCacheCreationOptimizationModeFree)
	require.True(t, changed)
	freeUsage, ok := extractOpenAIUsageFromJSONBytes(freeBody)
	require.True(t, ok)
	require.Equal(t, 476, max(freeUsage.InputTokens-freeUsage.CacheReadInputTokens-freeUsage.CacheCreationInputTokens, 0))
	require.Equal(t, 4736, freeUsage.CacheReadInputTokens)
	require.Zero(t, freeUsage.CacheCreationInputTokens)

	input125Body, changed := normalizeOpenAIDownstreamUsageJSON(body, OpenAIPromptCacheCreationOptimizationModeInput125)
	require.True(t, changed)
	input125Usage, ok := extractOpenAIUsageFromJSONBytes(input125Body)
	require.True(t, ok)
	require.Equal(t, 1102, max(input125Usage.InputTokens-input125Usage.CacheReadInputTokens-input125Usage.CacheCreationInputTokens, 0))
	require.Equal(t, 4756, input125Usage.CacheReadInputTokens)
	require.Equal(t, 22, input125Usage.OutputTokens)
	require.Zero(t, input125Usage.CacheCreationInputTokens)
}

func TestDownstreamSourceBilling_CIsFreeAndDPreservesGPT56CreationPrice(t *testing.T) {
	billing := NewBillingService(&config.Config{}, nil)
	actual, err := billing.CalculateCost("gpt-5.6-terra", UsageTokens{
		InputTokens: 476, OutputTokens: 20, CacheReadTokens: 4736, CacheCreationTokens: 512,
	}, 1)
	require.NoError(t, err)
	free, err := billing.CalculateCost("gpt-5.6-terra", UsageTokens{
		InputTokens: 476, OutputTokens: 20, CacheReadTokens: 4736, CacheCreationTokens: 0,
	}, 1)
	require.NoError(t, err)
	input125, err := billing.CalculateCost("gpt-5.6-terra", UsageTokens{
		InputTokens: 1102, OutputTokens: 22, CacheReadTokens: 4756, CacheCreationTokens: 0,
	}, 1)
	require.NoError(t, err)
	require.Zero(t, free.CacheCreationCost)
	require.Zero(t, input125.CacheCreationCost)
	require.InDelta(t, actual.TotalCost-actual.CacheCreationCost, free.TotalCost, 1e-12)
	require.InDelta(t, actual.TotalCost, input125.TotalCost, 1e-12,
		"the displayed input and creation buckets must preserve the original creation cost")
}

func TestNormalizeOpenAIResponsesUsageForDownstream_PreservesOriginalCopy(t *testing.T) {
	original := &apicompat.ResponsesUsage{
		InputTokens: 5724, OutputTokens: 20, TotalTokens: 5744,
		CacheCreationInputTokens: 512,
		InputTokensDetails:       &apicompat.ResponsesInputTokensDetails{CachedTokens: 4736, CacheWriteTokens: 512},
	}
	usage := *original
	details := *original.InputTokensDetails
	usage.InputTokensDetails = &details
	require.True(t, normalizeOpenAIResponsesUsageForDownstream(&usage, OpenAIPromptCacheCreationOptimizationModeFree))
	require.Equal(t, 5212, usage.InputTokens)
	require.Zero(t, usage.CacheCreationInputTokens)
	require.Zero(t, usage.InputTokensDetails.CacheWriteTokens)
	require.Equal(t, 5724, original.InputTokens)
	require.Equal(t, 512, original.CacheCreationInputTokens)
}

func TestNormalizeOpenAIResponsesUsageForDownstream_DCreatesMissingDetails(t *testing.T) {
	usage := &apicompat.ResponsesUsage{
		InputTokens: 988, OutputTokens: 20, TotalTokens: 1008,
		CacheCreationInputTokens: 512,
	}
	require.True(t, normalizeOpenAIResponsesUsageForDownstream(usage, OpenAIPromptCacheCreationOptimizationModeInput125))
	require.Equal(t, 1122, usage.InputTokens)
	require.Equal(t, 22, usage.OutputTokens)
	require.Equal(t, 1144, usage.TotalTokens)
	require.Zero(t, usage.CacheCreationInputTokens)
	require.NotNil(t, usage.InputTokensDetails)
	require.Equal(t, 20, usage.InputTokensDetails.CachedTokens)
}

func TestNormalizeOpenAIResponsesUsageForDownstream_ClampsMalformedNegativeBuckets(t *testing.T) {
	usage := &apicompat.ResponsesUsage{
		InputTokens: math.MinInt, OutputTokens: 1, TotalTokens: 0,
		CacheCreationInputTokens: 1,
		InputTokensDetails:       &apicompat.ResponsesInputTokensDetails{CachedTokens: -10, CacheWriteTokens: 1},
	}
	require.True(t, normalizeOpenAIResponsesUsageForDownstream(usage, OpenAIPromptCacheCreationOptimizationModeFree))
	require.Zero(t, usage.InputTokens)
	require.Equal(t, 1, usage.TotalTokens)
	require.Zero(t, usage.CacheCreationInputTokens)
	require.Zero(t, usage.InputTokensDetails.CachedTokens)
	require.Zero(t, usage.InputTokensDetails.CacheWriteTokens)
}

func TestMessagesDownstreamDisplay_UsesAnthropicExclusiveInputSemantics(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		wantInput int
	}{
		{OpenAIPromptCacheCreationOptimizationModeFree, 476},
		{OpenAIPromptCacheCreationOptimizationModeInput125, 1102},
	} {
		response := &apicompat.ResponsesResponse{
			ID: "resp_usage", Model: "gpt-5.6-terra", Status: "completed",
			Usage: &apicompat.ResponsesUsage{
				InputTokens: 5724, OutputTokens: 20, TotalTokens: 5744,
				CacheCreationInputTokens: 512,
				InputTokensDetails:       &apicompat.ResponsesInputTokensDetails{CachedTokens: 4736, CacheWriteTokens: 512},
			},
		}
		require.True(t, normalizeOpenAIResponsesUsageForDownstream(response.Usage, tc.mode))
		anthropic := apicompat.ResponsesToAnthropic(response, "gpt-5.6-terra")
		require.Equal(t, tc.wantInput, anthropic.Usage.InputTokens)
		if tc.mode == OpenAIPromptCacheCreationOptimizationModeInput125 {
			require.Equal(t, 4756, anthropic.Usage.CacheReadInputTokens)
			require.Equal(t, 22, anthropic.Usage.OutputTokens)
			require.Zero(t, anthropic.Usage.CacheCreationInputTokens)
		} else {
			require.Equal(t, 4736, anthropic.Usage.CacheReadInputTokens)
			require.Zero(t, anthropic.Usage.CacheCreationInputTokens)
		}
	}
}
