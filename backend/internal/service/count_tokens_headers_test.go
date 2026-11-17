package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestV0222CountTokensPreservesAllBetaValuesWithoutDuplicateAuth(t *testing.T) {
	ctx := context.Background()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages/count_tokens", nil)
	c.Request.Header = http.Header{"Anthropic-Beta": {"first-beta", "second-beta"}, "anthropic-beta": {"third-beta,first-beta"}, "X-Api-Key": {"client-secret", "other-secret"}}
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	svc := &GatewayService{cfg: &config.Config{}}
	req, err := svc.buildCountTokensRequestAnthropicAPIKeyPassthrough(ctx, c, account, []byte(`{"model":"claude-sonnet-4","messages":[]}`), "account-secret")
	require.NoError(t, err)
	for _, beta := range []string{"first-beta", "second-beta", "third-beta"} {
		require.Contains(t, getHeaderRaw(req.Header, "anthropic-beta"), beta)
	}
	require.Equal(t, "account-secret", getHeaderRaw(req.Header, "x-api-key"))
	for key, values := range req.Header {
		require.Len(t, values, 1, key)
	}
	beta, ok := svc.computeFinalCountTokensAnthropicBeta("oauth", false, "claude-sonnet-4", c.Request.Header, nil, nil)
	require.True(t, ok)
	require.Contains(t, beta, "second-beta")
	require.Contains(t, beta, "third-beta")
}
