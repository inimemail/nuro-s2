package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthCombinedControlsPreserveFailureBoundary(t *testing.T) {
	setGinTestMode()
	for _, passthrough := range []bool{false, true} {
		for _, preamble := range []bool{false, true} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
				openAIOAuthChatGPTPreambleFlushExtraKey:                            preamble,
				openAIOAuthChatGPTSSECommentPreflushExtraKey:                       true,
				openAIOAuthChatGPTSafeTokenPlaceholderExtraKey:                     true,
				openAIOAuthChatGPTFirstTokenTimeoutPlaceholderEnabledExtraKey:      true,
				openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:           600,
				openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardEnabledExtraKey: true,
				openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey:   30000,
			}}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed\"}}\n\n" +
					"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"upstream failure\"}}}\n\n"))}
			svc := &OpenAIGatewayService{}
			var err error
			if passthrough {
				_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6-sol", "gpt-5.6-sol")
			} else {
				_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6-sol", "gpt-5.6-sol")
			}
			require.Error(t, err)
			var failover *UpstreamFailoverError
			if preamble {
				require.NotErrorAs(t, err, &failover)
				require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
				require.False(t, OpenAIRequestAllowsFailover(c, -1))
			} else {
				require.ErrorAs(t, err, &failover)
				require.NotContains(t, rec.Body.String(), "resp_failed")
				require.True(t, OpenAIRequestAllowsFailover(c, -1))
			}
			_, sampled := svc.openaiFirstTokenTimeoutPlaceholderGuard.latestRealTokenMS(account.ID, "gpt-5.6-sol")
			require.False(t, sampled, "failed attempts must not overwrite a successful real-token sample")
		}
	}
}

// Exercise the controls together: tests for each switch alone do not catch
// preamble forwarding being overridden by placeholder coordination.
func TestOpenAIOAuthHTTPStreamFeaturesWorkTogether(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) { testOpenAIHTTPStreamFeaturesWorkTogether(t, accountType) })
	}
}

func testOpenAIHTTPStreamFeaturesWorkTogether(t *testing.T, accountType string) {
	setGinTestMode()
	for _, passthrough := range []bool{false, true} {
		name := "converted"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
				openAIOAuthChatGPTPreambleFlushExtraKey:                            true,
				openAIOAuthChatGPTSSECommentPreflushExtraKey:                       true,
				openAIOAuthChatGPTSafeTokenPlaceholderExtraKey:                     true,
				openAIOAuthChatGPTFirstTokenTimeoutPlaceholderEnabledExtraKey:      true,
				openAIOAuthChatGPTFirstTokenTimeoutPlaceholderMsExtraKey:           20,
				openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardEnabledExtraKey: true,
				openAIOAuthChatGPTFirstTokenTimeoutPlaceholderGuardMaxMsExtraKey:   30000,
			}}
			account.Type = accountType
			if accountType == AccountTypeAPIKey {
				extra := make(map[string]any, len(account.Extra))
				for key, value := range account.Extra {
					extra[strings.Replace(key, "openai_oauth_chatgpt_", "openai_apikey_", 1)] = value
				}
				account.Extra = extra
			}
			svc := &OpenAIGatewayService{}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &delayedSSEChunkReadCloser{chunks: []delayedSSEChunk{
				{data: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_features\"}}\n\n"},
				{data: "data: {\"type\":\"response.in_progress\"}\n\n"},
				{delay: 60 * time.Millisecond, data: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":2}}}\n\n"},
			}}}
			var firstToken *int
			var usage *OpenAIUsage
			var err error
			if passthrough {
				var result *openaiStreamingResultPassthrough
				result, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6-sol", "gpt-5.6-sol")
				require.NotNil(t, result)
				firstToken, usage = result.firstTokenMs, result.usage
			} else {
				var result *openaiStreamingResult
				result, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6-sol", "gpt-5.6-sol")
				require.NotNil(t, result)
				firstToken, usage = result.firstTokenMs, result.usage
			}
			require.NoError(t, err)
			body := rec.Body.String()
			require.True(t, strings.HasPrefix(body, ":\n\n"), body)
			progress := `"type":"response.transport_progress.delta"`
			require.Equal(t, 2, strings.Count(body, progress), "safe and timeout controls must both work")
			require.Less(t, strings.Index(body, `"type":"response.created"`), strings.Index(body, progress), "the preamble switch must still forward created before the safe frame")
			require.Less(t, strings.Index(body, `"type":"response.in_progress"`), strings.LastIndex(body, progress))
			require.Less(t, strings.LastIndex(body, progress), strings.Index(body, `"type":"response.output_text.delta"`))
			require.NotNil(t, firstToken)
			require.GreaterOrEqual(t, *firstToken, 40)
			require.Equal(t, 12, usage.InputTokens)
			require.Equal(t, 2, usage.OutputTokens)
			require.Equal(t, 1, strings.Count(body, `"type":"response.completed"`))
			sample, ok := svc.openaiFirstTokenTimeoutPlaceholderGuard.latestRealTokenMS(account.ID, "gpt-5.6-sol")
			require.True(t, ok)
			require.GreaterOrEqual(t, sample, 40, "protection must learn real output latency, not the placeholder")
		})
	}
}
