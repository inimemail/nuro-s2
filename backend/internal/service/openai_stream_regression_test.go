package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAILargeCreatedPreservesPlaceholders(t *testing.T) {
	setGinTestMode()
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, passthrough := range []bool{false, true} {
			for _, size := range []int{10, 6000} {
				name := "converted"
				if passthrough {
					name = "passthrough"
				}
				if size > 100 {
					name += "/large"
				} else {
					name += "/small"
				}
				t.Run(accountType+"/"+name, func(t *testing.T) {
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
					created, err := json.Marshal(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_large", "instructions": strings.Repeat("x", size)}})
					require.NoError(t, err)
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &delayedSSEChunkReadCloser{chunks: []delayedSSEChunk{
						{data: "event: response.created\ndata: " + string(created) + "\n\n"},
						{data: "data: {\"type\":\"response.in_progress\"}\n\n"},
						{delay: 100 * time.Millisecond, data: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"},
					}}}
					svc := &OpenAIGatewayService{}
					if passthrough {
						_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6-sol", "gpt-5.6-sol")
					} else {
						_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6-sol", "gpt-5.6-sol")
					}
					require.NoError(t, err)
					require.Equal(t, 2, strings.Count(rec.Body.String(), `"type":"response.transport_progress.delta"`), "created size=%d: safe and timeout frames must survive structural preamble", size)
				})
			}
		}
	}
}

func TestOpenAINativeProbeMustNotDisableStandaloneCompact(t *testing.T) {
	setGinTestMode()
	updatesCh := make(chan map[string]any, 1)
	account := Account{ID: 91, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-key"}, Extra: map[string]any{"openai_compact_supported": true}}
	repo := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{account}}, updateExtraCalls: updatesCh}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n"))}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/91/test", nil)
	err := svc.TestAccountConnection(c, account.ID, "gpt-5.6-sol", "", AccountTestModeCompact)
	require.Error(t, err)
	require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)
	updates := <-updatesCh
	mergeAccountExtra(&account, updates)
	require.True(t, account.AllowsOpenAICompact(), "a native /responses probe must not revoke previously established /responses/compact capability")
}

func TestOpenAIHistoricalNativeFalseNegativeSurvivesTransientReprobe(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{
		"openai_compact_supported":  false,
		"openai_compact_last_error": "native remote compaction v2 unsupported",
	}}
	updates := buildOpenAICompactProbeUpdatesForMode(&http.Response{StatusCode: http.StatusServiceUnavailable}, nil, nil, false, false, account)
	mergeAccountExtra(account, updates)
	require.True(t, account.AllowsOpenAICompact())
}
