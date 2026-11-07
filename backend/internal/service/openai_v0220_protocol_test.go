//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesInstructionsEchoPreservesClientIntent(t *testing.T) {
	for _, original := range []string{`{}`, `{"instructions":null}`, `{"instructions":""}`, `{"instructions":"client\n文本"}`} {
		t.Run(original, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			rememberOpenAIResponsesInstructions(c, []byte(original))
			syncOpenAIResponsesInstructions(c, []byte(`{"instructions":"injected"}`))
			for _, path := range []string{"instructions", "response.instructions"} {
				body := []byte(`{"instructions":"injected","usage":{"input_tokens":7}}`)
				if path != "instructions" {
					body = []byte(`{"response":{"instructions":"injected","usage":{"input_tokens":7}}}`)
				}
				restored := restoreOpenAIResponsesInstructions(c, body, path)
				actual, expected := gjson.GetBytes(restored, path), gjson.Get(original, "instructions")
				require.Equal(t, expected.Exists(), actual.Exists())
				require.Equal(t, expected.Raw, actual.Raw)
				require.NotContains(t, string(restored), "injected")
				require.Contains(t, string(restored), `"input_tokens":7`)
			}
		})
	}
}
func TestOAuthNonStreamMissingContentTypeSSETerminals(t *testing.T) {
	for _, terminal := range []string{"completed", "incomplete", "cancelled", "canceled"} {
		t.Run(terminal, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			rememberOpenAIResponsesInstructions(c, []byte(`{"stream":false}`))
			syncOpenAIResponsesInstructions(c, []byte(`{"stream":true,"instructions":"internal"}`))
			raw := `data: {"type":"response.` + terminal + `","response":{"id":"resp_1","object":"response","status":"` + terminal + `","instructions":"internal","output":[],"usage":{"input_tokens":7,"output_tokens":2}}}` + "\n\n"
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "gpt-6.1-sol", "")
			require.NoError(t, err)
			require.Equal(t, 7, result.InputTokens)
			require.Equal(t, 2, result.OutputTokens)
			require.Equal(t, "response."+terminal, result.terminalEventType)
			require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			require.True(t, gjson.Valid(rec.Body.String()))
			require.False(t, gjson.Get(rec.Body.String(), "instructions").Exists())
		})
	}
}

func TestTerraV0220EffortAndCatalogBoundaries(t *testing.T) {
	for _, model := range []string{"gpt-6.1-terra", "OPENAI/GPT-6.1-TERRA", "models/gpt_6.1_terra"} {
		for _, effort := range []string{"minimal", "ultra"} {
			body := []byte(`{"model":"` + model + `","reasoning":{"effort":"` + effort + `"}}`)
			require.Error(t, validateGPT61SolRequestBody(body, model))
			_, err := normalizeGPT6ResponsesSampling(body)
			require.Error(t, err)
		}
		body := []byte(`{"model":"` + model + `","reasoning":{"effort":"none"},"temperature":0.5,"prompt_cache_key":"stable","prompt_cache_retention":"24h"}`)
		out, err := normalizeGPT6ResponsesSampling(body)
		require.NoError(t, err)
		require.Equal(t, body, out)
	}
	require.Error(t, validateGPT61SolRequestBody([]byte(`{}`), "gpt-6.1-terra-minimal"))
	require.Error(t, validateGPT61SolRequestBody([]byte(`{"reasoning_effort":"none"}`), "gpt-6.1-sol"))
	catalog := []byte(`{"models":[{"slug":"gpt-6.1-sol"},{"slug":"gpt-6.1-terra","use_responses_lite":true,"context_window":12345,"custom":"preserve"}]}`)
	out := convertOpenAIModelListToCodexManifest(catalog)
	require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(out, "models.0.slug").String())
	require.Equal(t, "gpt-6.1-terra", gjson.GetBytes(out, "models.1.slug").String())
	require.EqualValues(t, 12345, gjson.GetBytes(out, "models.1.context_window").Int())
	require.True(t, gjson.GetBytes(out, "models.1.use_responses_lite").Bool())
	require.Contains(t, gjson.GetBytes(out, "models.1.supported_reasoning_levels.#.effort").Raw, "ultra")
	for _, field := range []string{"base_instructions", "model_instructions", "priority", "code_mode_only", "prompt_cache_key"} {
		require.False(t, gjson.GetBytes(out, "models.1."+field).Exists(), field)
	}
	apiKey, err := adjustAPIKeyCodexModelsManifest(out)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(apiKey, "models.1.use_responses_lite").Bool())
	require.Equal(t, "preserve", gjson.GetBytes(apiKey, "models.1.custom").String())
}

func TestTerraV0220RecordUsageOAuthLongContextExemption(t *testing.T) {
	for _, typ := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		t.Run(typ, func(t *testing.T) {
			repo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			defer svc.StopBillingNotifications()
			raw := OpenAIUsage{InputTokens: 300000, OutputTokens: 2000, CacheReadInputTokens: 100000}
			result := &OpenAIForwardResult{RequestID: "terra_exemption_" + typ, Model: "gpt-6.1-terra", UpstreamModel: "gpt-6.1-terra", Usage: raw}
			err := svc.RecordUsage(t.Context(), &OpenAIRecordUsageInput{
				Result: result, APIKey: &APIKey{ID: 9901}, User: &User{ID: 9902},
				Account: &Account{ID: 9903, Type: typ, Platform: PlatformOpenAI, Extra: map[string]any{openAILongContextBillingEnabledKey: true}},
			})
			require.NoError(t, err)
			require.NotNil(t, repo.lastLog)
			multiplier, outputMultiplier := 1.0, 1.0
			if typ == AccountTypeAPIKey {
				multiplier, outputMultiplier = 2, 1.5
			}
			want := (200000*1e-6+100000*5e-8)*multiplier + 2000*5e-6*outputMultiplier
			require.InDelta(t, want, repo.lastLog.TotalCost, 1e-12)
			require.Equal(t, typ == AccountTypeAPIKey, repo.lastLog.LongContextBillingApplied)
			require.Equal(t, raw, result.Usage)
		})
	}
}

func TestAntigravityV0220AdminPromptReachesMappedUpstream(t *testing.T) {
	for _, model := range []string{"gemini-4-argon-high", "claude-sonnet-4-5"} {
		t.Run(model, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + `{"response":{"candidates":[{"content":{"parts":[{"text":"actual reply"}]}}]}}` + "\n\n"))}}
			svc := &AntigravityGatewayService{tokenProvider: &AntigravityTokenProvider{}, httpUpstream: upstream}
			account := &Account{ID: 9904, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture", "project_id": "fixture-project", "model_mapping": map[string]any{"public-test": model}}}
			prompt := "  管理员输入\n\"quoted\"  "
			result, err := svc.TestConnectionWithPrompt(t.Context(), account, "public-test", prompt)
			require.NoError(t, err)
			require.Equal(t, "actual reply", result.Text)
			require.Equal(t, model, result.MappedModel)
			require.Len(t, upstream.requests, 1)
			wantPrompt := prompt
			if strings.HasPrefix(model, "claude-") {
				// The existing Claude-to-Gemini text normalization trims edges.
				wantPrompt = strings.TrimSpace(prompt)
			}
			require.Equal(t, wantPrompt, gjson.GetBytes(upstream.lastBody, "request.contents.0.parts.0.text").String())
			require.EqualValues(t, 1024, gjson.GetBytes(upstream.lastBody, "request.generationConfig.maxOutputTokens").Int())
		})
	}
}
