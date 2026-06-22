package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAnthropicCacheBoostIndependentFromOpenAIFields(t *testing.T) {
	account := &Account{
		ID:       7101,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"prompt_cache_boost_enabled": true,
		},
	}

	require.False(t, account.IsAnthropicCacheBoostEnabled())
	require.False(t, account.IsAnthropicUpstreamStrongIsolationEnabled())

	account.Credentials["anthropic_cache_boost_enabled"] = true
	account.Credentials["anthropic_cache_boost_level"] = AnthropicCacheBoostLevelAggressive
	account.Credentials["anthropic_upstream_strong_isolation_enabled"] = true

	require.True(t, account.IsAnthropicCacheBoostEnabled())
	require.True(t, account.IsAnthropicCacheBoostAggressive())
	require.True(t, account.IsAnthropicUpstreamStrongIsolationEnabled())
}

func TestAnthropicCacheBoostAPIKeyRequiresPoolMode(t *testing.T) {
	account := &Account{
		ID:       7104,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"anthropic_cache_boost_enabled":               true,
			"anthropic_upstream_strong_isolation_enabled": true,
		},
	}

	require.False(t, account.IsAnthropicCacheBoostEnabled())
	require.False(t, account.IsAnthropicUpstreamStrongIsolationEnabled())

	account.Credentials["pool_mode"] = true
	require.True(t, account.IsAnthropicCacheBoostEnabled())
	require.True(t, account.IsAnthropicUpstreamStrongIsolationEnabled())
}

func TestAnthropicCacheBoostAuthorizedAccounts(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		account := &Account{
			ID:       7105,
			Platform: PlatformAnthropic,
			Type:     accountType,
			Credentials: map[string]any{
				"anthropic_cache_boost_enabled":               true,
				"anthropic_upstream_strong_isolation_enabled": true,
			},
		}

		require.True(t, account.IsAnthropicCacheBoostEnabled(), accountType)
		require.True(t, account.IsAnthropicUpstreamStrongIsolationEnabled(), accountType)
	}
}

func TestApplyAnthropicCacheBoostBody_SmallRequestUnchanged(t *testing.T) {
	account := &Account{
		ID:          7102,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"pool_mode": true, "anthropic_cache_boost_enabled": true},
	}
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`)
	svc := &GatewayService{}

	got := svc.applyAnthropicCacheBoostBody(context.Background(), account, body)

	require.JSONEq(t, string(body), string(got))
}

func TestApplyAnthropicCacheBoostBody_AggressiveInjectsStableBreakpoints(t *testing.T) {
	account := &Account{
		ID:       7103,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                     true,
			"anthropic_cache_boost_enabled": true,
			"anthropic_cache_boost_level":   AnthropicCacheBoostLevelAggressive,
		},
	}
	longText := strings.Repeat("stable-prefix ", 500)
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":"` + longText + `",
		"tools":[{"name":"tool_a","description":"` + longText + `","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"` + longText + ` one"},
			{"role":"assistant","content":"ok"},
			{"role":"user","content":"` + longText + ` two"},
			{"role":"assistant","content":"ok"},
			{"role":"user","content":"` + longText + ` three"}
		]
	}`)
	svc := &GatewayService{}

	got := svc.applyAnthropicCacheBoostBody(context.Background(), account, body)

	require.True(t, gjson.GetBytes(got, "system.0.cache_control").Exists())
	require.True(t, gjson.GetBytes(got, "messages.4.content.0.cache_control").Exists())
	_, messagePaths, toolPaths, systemPaths := collectCacheControlPaths(got)
	require.LessOrEqual(t, len(messagePaths)+len(toolPaths)+len(systemPaths), maxCacheControlBlocks)
}

func TestAnthropicUpstreamStrongIsolationBodyKeepsCacheControl(t *testing.T) {
	body := []byte(`{
		"session_id":"sess_1",
		"conversation_id":"conv_1",
		"client_metadata":{"x-codex-turn-state":"state"},
		"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	got, changed, err := applyAnthropicUpstreamStrongIsolationBody(body)

	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(got, "session_id").Exists())
	require.False(t, gjson.GetBytes(got, "conversation_id").Exists())
	require.False(t, gjson.GetBytes(got, "client_metadata").Exists())
	require.True(t, gjson.GetBytes(got, "messages.0.content.0.cache_control").Exists())
}

func TestAnthropicUpstreamStrongIsolationHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	require.NoError(t, err)
	req.Header.Set("Session_ID", "sess_1")
	req.Header.Set("Conversation_ID", "conv_1")
	req.Header.Set("X-Claude-Code-Session-Id", "claude_sess_1")
	req.Header.Set("X-Codex-Turn-State", "state")
	req.Header.Set("Anthropic-Version", "2023-06-01")

	applyAnthropicUpstreamStrongIsolationHeaders(req)

	require.Empty(t, req.Header.Get("Session_ID"))
	require.Empty(t, req.Header.Get("Conversation_ID"))
	require.Empty(t, req.Header.Get("X-Claude-Code-Session-Id"))
	require.Empty(t, req.Header.Get("X-Codex-Turn-State"))
	require.Equal(t, "2023-06-01", req.Header.Get("Anthropic-Version"))
}

func TestAnthropicBuildUpstreamRequestStrongIsolationStripsClientSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	c.Request.Header.Set("Session_ID", "sess_1")
	c.Request.Header.Set("Conversation_ID", "conv_1")
	c.Request.Header.Set("X-Claude-Code-Session-Id", "claude_sess_1")
	c.Request.Header.Set("X-Codex-Turn-State", "state")
	c.Request.Header.Set("Anthropic-Version", "2023-06-01")

	account := &Account{
		ID:       7106,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode": true,
			"anthropic_upstream_strong_isolation_enabled": true,
		},
	}
	svc := &GatewayService{cfg: &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, body, "sk-test", "apikey", "claude-sonnet-4-6", false, false)
	require.NoError(t, err)
	require.Empty(t, req.Header.Get("Session_ID"))
	require.Empty(t, req.Header.Get("Conversation_ID"))
	require.Empty(t, req.Header.Get("X-Claude-Code-Session-Id"))
	require.Empty(t, req.Header.Get("X-Codex-Turn-State"))
	require.Equal(t, "2023-06-01", getHeaderRaw(req.Header, "anthropic-version"))
	require.Equal(t, "sk-test", getHeaderRaw(req.Header, "x-api-key"))
}
