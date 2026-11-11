//go:build unit

package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCompatForwardersHonorExplicitErrorPolicyWithoutWideningScope(t *testing.T) {
	setGinTestMode()
	for _, protocol := range []string{"chat", "responses"} {
		for _, scope := range []string{"", "account", "model"} {
			t.Run(protocol+"/"+scope, func(t *testing.T) {
				repo := &tempModelScopeTestRepo{}
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.Enabled = false
				upstream := &geminiSequenceUpstreamStub{responses: []geminiSequenceUpstreamResponse{{status: http.StatusTeapot, body: `{"error":{"message":"fixture busy https://private.invalid token-secret"}}`}}}
				svc := &GatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: NewRateLimitService(repo, nil, cfg, nil, nil)}
				account := &Account{ID: 81, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{
					"api_key": "sk-fixture", "base_url": "https://upstream.invalid", "model_mapping": map[string]any{"public-model": "claude-sonnet-4-6"},
				}}
				if scope != "" {
					account.Credentials["temp_unschedulable_enabled"] = true
					account.Credentials["temp_unschedulable_rules"] = []any{map[string]any{"error_code": 418, "keywords": []any{"fixture busy"}, "duration_minutes": 1, "scope": scope}}
				}
				body := []byte(`{"model":"public-model","messages":[{"role":"user","content":"hello"}]}`)
				if protocol == "responses" {
					body = []byte(`{"model":"public-model","input":"hello"}`)
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+protocol, bytes.NewReader(body))
				var err error
				if protocol == "chat" {
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				} else {
					_, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
				}
				require.Error(t, err)
				require.Equal(t, 1, upstream.calls, "the forwarder must not execute another generation")
				var failover *UpstreamFailoverError
				if scope == "" {
					require.NotErrorAs(t, err, &failover)
					require.NotContains(t, rec.Body.String(), "private.invalid")
					require.NotContains(t, rec.Body.String(), "token-secret")
				} else {
					require.ErrorAs(t, err, &failover)
					require.Equal(t, http.StatusTeapot, failover.StatusCode)
					require.Empty(t, rec.Body.String(), "failover must not commit an error response")
				}
				if scope == "model" {
					require.Equal(t, []string{"claude-sonnet-4-6"}, repo.models)
				} else {
					require.Empty(t, repo.models)
				}
				if scope == "account" {
					require.Equal(t, 1, repo.accountCalls)
				} else {
					require.Zero(t, repo.accountCalls)
				}
			})
		}
	}
}
