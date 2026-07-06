package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestAnthropicCacheBoostUpstreamHitPriorityRequiresAggressiveBoost(t *testing.T) {
	account := &Account{
		ID:       7131,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"anthropic_cache_boost_upstream_hit_priority_enabled": true,
		},
	}

	require.False(t, account.IsAnthropicCacheBoostUpstreamHitPriorityEnabled())

	account.Credentials["anthropic_cache_boost_enabled"] = true
	require.False(t, account.IsAnthropicCacheBoostUpstreamHitPriorityEnabled())

	account.Credentials["anthropic_cache_boost_level"] = AnthropicCacheBoostLevelAggressive
	require.True(t, account.IsAnthropicCacheBoostUpstreamHitPriorityEnabled())
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

func TestAnthropicCacheBoostUpstreamAffinityHashUsesStaticPrefix(t *testing.T) {
	system := strings.Repeat("stable-system ", 120)
	bodyA := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":"` + system + `",
		"tools":[{"name":"tool_a","description":"` + system + `","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"first user question"}]
	}`)
	bodyB := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":"` + system + `",
		"tools":[{"name":"tool_a","description":"` + system + `","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"different user question"}]
	}`)
	bodyC := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":"` + system + ` changed",
		"tools":[{"name":"tool_a","description":"` + system + `","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"first user question"}]
	}`)

	hashA := DeriveAnthropicCacheBoostUpstreamAffinityHash("claude-sonnet-4-6", bodyA)
	hashB := DeriveAnthropicCacheBoostUpstreamAffinityHash("claude-sonnet-4-6", bodyB)
	hashC := DeriveAnthropicCacheBoostUpstreamAffinityHash("claude-sonnet-4-6", bodyC)

	require.NotEmpty(t, hashA)
	require.True(t, IsAnthropicCacheBoostUpstreamAffinitySessionHash(hashA))
	require.Equal(t, hashA, hashB)
	require.NotEqual(t, hashA, hashC)
	require.Empty(t, DeriveAnthropicCacheBoostUpstreamAffinityHash("claude-sonnet-4-6", []byte(`{"model":"claude-sonnet-4-6","system":"short"}`)))
}

func TestDeriveAnthropicCacheAffinityStickyTTLIgnoresMessageTTLWhenBoostRewritesBody(t *testing.T) {
	longSystem := strings.Repeat("stable-prefix ", 400)
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":"` + longSystem + `",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	require.GreaterOrEqual(t, len(body), anthropicCacheBoostAggressiveMinBodyBytes)
	require.Equal(t, anthropicCacheAffinityStickyTTLDefault, DeriveAnthropicCacheAffinityStickyTTL(body))
}

func TestDeriveAnthropicCacheAffinityStickyTTLKeepsDurableOrUnrewrittenTTL(t *testing.T) {
	longText := strings.Repeat("stable-prefix ", 400)
	systemTTLBody := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":[{"type":"text","text":"` + longText + `","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"messages":[{"role":"user","content":"hello"}]
	}`)
	smallMessageTTLBody := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	require.GreaterOrEqual(t, len(systemTTLBody), anthropicCacheBoostAggressiveMinBodyBytes)
	require.Less(t, len(smallMessageTTLBody), anthropicCacheBoostAggressiveMinBodyBytes)
	require.Equal(t, anthropicCacheAffinityStickyTTLExtended, DeriveAnthropicCacheAffinityStickyTTL(systemTTLBody))
	require.Equal(t, anthropicCacheAffinityStickyTTLExtended, DeriveAnthropicCacheAffinityStickyTTL(smallMessageTTLBody))
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

func TestApplyAnthropicCacheBoostBody_UpstreamPriorityUsesStableLongConversationBreakpoints(t *testing.T) {
	account := &Account{
		ID:       7132,
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"pool_mode":                                           true,
			"anthropic_cache_boost_enabled":                       true,
			"anthropic_cache_boost_level":                         AnthropicCacheBoostLevelAggressive,
			"anthropic_cache_boost_upstream_hit_priority_enabled": true,
		},
	}
	longText := strings.Repeat("stable-prefix ", 500)
	body := []byte(`{
		"model":"claude-sonnet-4-6",
		"system":"` + longText + `",
		"tools":[{"name":"tool_a","description":"` + longText + `","input_schema":{"type":"object"}}],
		"messages":[
			{"role":"user","content":"q1 ` + longText + `"},
			{"role":"assistant","content":"a1"},
			{"role":"user","content":"q2 ` + longText + `"},
			{"role":"assistant","content":"a2"},
			{"role":"user","content":"q3 ` + longText + `"},
			{"role":"assistant","content":"a3"},
			{"role":"user","content":"q4 ` + longText + `"},
			{"role":"assistant","content":"a4"}
		]
	}`)
	svc := &GatewayService{}

	got := svc.applyAnthropicCacheBoostBody(context.Background(), account, body)

	require.True(t, gjson.GetBytes(got, "system.0.cache_control").Exists())
	require.True(t, gjson.GetBytes(got, "tools.0.cache_control").Exists())
	require.True(t, gjson.GetBytes(got, "messages.2.content.0.cache_control").Exists())
	require.True(t, gjson.GetBytes(got, "messages.4.content.0.cache_control").Exists())
	require.False(t, gjson.GetBytes(got, "messages.7.content.0.cache_control").Exists())
	_, messagePaths, toolPaths, systemPaths := collectCacheControlPaths(got)
	require.LessOrEqual(t, len(messagePaths)+len(toolPaths)+len(systemPaths), maxCacheControlBlocks)
}

func TestAnthropicCacheBoostUpstreamAffinitySelectionUsesSameLayerOnly(t *testing.T) {
	now := time.Now()
	oldLastUsed := now.Add(-30 * time.Minute)
	newLastUsed := now.Add(-1 * time.Minute)
	accounts := []*Account{
		anthropicAffinityTestAccount(1, 1, &oldLastUsed, true),
		anthropicAffinityTestAccount(2, 1, &newLastUsed, true),
	}

	selected := selectLayeredAccountWithAnthropicAffinity(accounts, nil, config.GatewaySchedulingConfig{}, false, now, 2, true)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.ID)

	highPriority := anthropicAffinityTestAccount(3, 0, &oldLastUsed, true)
	selected = selectLayeredAccountWithAnthropicAffinity(append(accounts, highPriority), nil, config.GatewaySchedulingConfig{}, false, now, 2, true)
	require.NotNil(t, selected)
	require.Equal(t, int64(3), selected.ID, "affinity must not cross priority layers")
}

func TestAnthropicCacheBoostUpstreamAffinityIgnoresLoadLayerAndRespectsToggle(t *testing.T) {
	now := time.Now()
	oldLastUsed := now.Add(-30 * time.Minute)
	newLastUsed := now.Add(-1 * time.Minute)
	preferredByLoad := anthropicAffinityTestAccount(1, 1, &oldLastUsed, true)
	affinityTarget := anthropicAffinityTestAccount(2, 1, &newLastUsed, true)
	disabledTarget := anthropicAffinityTestAccount(3, 1, &newLastUsed, false)

	selected := selectLayeredAccountWithLoadAndAnthropicAffinity([]accountWithLoad{
		{account: preferredByLoad, loadInfo: &AccountLoadInfo{LoadRate: 10}},
		{account: affinityTarget, loadInfo: &AccountLoadInfo{LoadRate: 60}},
	}, nil, config.GatewaySchedulingConfig{}, false, now, 2, true)
	require.NotNil(t, selected)
	require.Equal(t, int64(2), selected.account.ID, "load must not block same-priority upstream cache affinity")

	selected = selectLayeredAccountWithLoadAndAnthropicAffinity([]accountWithLoad{
		{account: preferredByLoad, loadInfo: &AccountLoadInfo{LoadRate: 10}},
		{account: disabledTarget, loadInfo: &AccountLoadInfo{LoadRate: 10}},
	}, nil, config.GatewaySchedulingConfig{}, false, now, 3, true)
	require.NotNil(t, selected)
	require.Equal(t, int64(1), selected.account.ID, "accounts without the toggle must not receive affinity preference")
}

func anthropicAffinityTestAccount(id int64, priority int, lastUsedAt *time.Time, upstreamPriority bool) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeOAuth,
		Priority:    priority,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 5,
		LastUsedAt:  lastUsedAt,
		Credentials: map[string]any{
			"anthropic_cache_boost_enabled":                       true,
			"anthropic_cache_boost_level":                         AnthropicCacheBoostLevelAggressive,
			"anthropic_cache_boost_upstream_hit_priority_enabled": upstreamPriority,
		},
	}
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
