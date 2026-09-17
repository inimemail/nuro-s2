package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountTestService_OrdinaryOAuthProbeUsesCurrentCodexHeaders(t *testing.T) {
	setGinTestMode()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(successfulOpenAIResponsesSSE())),
	}}
	svc := &AccountTestService{httpUpstream: upstream}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/123/test", nil)
	account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-account"}}
	require.NoError(t, svc.testOpenAIAccountConnection(c, account, "gpt-5.6-sol", "hi", AccountTestModeDefault))
	require.Equal(t, chatgptCodexAPIURL, upstream.lastReq.URL.String())
	require.Empty(t, upstream.lastReq.Header.Get("OpenAI-Beta"))
	require.Contains(t, upstream.lastReq.Header.Get("x-codex-beta-features"), openAIRemoteCompactionV2Feature)
	require.Equal(t, "Bearer oauth-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "chatgpt-account", upstream.lastReq.Header.Get("chatgpt-account-id"))
}
