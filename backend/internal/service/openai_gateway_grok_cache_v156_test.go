package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newGrokV156CacheContext(apiKeyID int64) *gin.Context {
	c, _ := gin.CreateTestContext(nil)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: apiKeyID})
	return c
}

func TestResolveGrokCacheIdentityUsesStableCustomPrefix(t *testing.T) {
	c := newGrokV156CacheContext(101)
	first := []byte(`{"model":"grok-4.5","instructions":"stable","input":"first"}`)
	second := []byte(`{"model":"grok-4.5","instructions":"stable","input":"second"}`)
	firstIdentity := resolveGrokCacheIdentity(c, first, "", "grok-4.5")
	require.NotEmpty(t, firstIdentity)
	require.Equal(t, firstIdentity, resolveGrokCacheIdentity(c, second, "", "grok-4.5"))
}

func TestResolveGrokCacheIdentityPrefersClaudeCodeSession(t *testing.T) {
	c := newGrokV156CacheContext(103)
	c.Request.Header.Set(claudeCodeSessionHeader, "session-a")
	first := resolveGrokCacheIdentity(c, []byte(`{"model":"grok-4.5","input":"first"}`), "", "grok-4.5")
	c.Request.Header.Set(claudeCodeSessionHeader, "session-b")
	second := resolveGrokCacheIdentity(c, []byte(`{"model":"grok-4.5","input":"first"}`), "", "grok-4.5")
	require.NotEmpty(t, first)
	require.NotEqual(t, first, second)
}

func TestExtractClaudeCodeSessionIDFromMetadata(t *testing.T) {
	require.Equal(t, "abc-123", extractClaudeCodeSessionIDFromPayload([]byte(`{"metadata":{"user_id":"user_session_abc-123"}}`)))
	require.Equal(t, "json-session", extractClaudeCodeSessionIDFromPayload([]byte(`{"metadata":{"user_id":"{\"session_id\":\"json-session\"}"}}`)))
}

func TestResolveGrokCacheIdentityRejectsModelOnlyFallback(t *testing.T) {
	require.Empty(t, resolveGrokCacheIdentity(
		newGrokV156CacheContext(102),
		[]byte(`{"model":"grok-4.5"}`),
		"",
		"grok-4.5",
	))
}

func TestApplyGrokFreeMessagesFunctionToolCacheRoute(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"subscription_tier": "free",
		},
	}
	body := []byte(`{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, body, account, "isolated")
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(patched, "tools").Array(), 3)
}

func TestApplyGrokFreeMessagesFunctionToolCacheRouteSkipsPaid(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"subscription_tier": "SuperGrok",
		},
	}
	body := []byte(`{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, body, account, "isolated")
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(patched))
}

func TestApplyGrokFreeRequestToolCacheRouteHonorsRequestOverride(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"subscription_tier": "free",
		},
		Extra: map[string]any{grokClientToolCacheExtraKey: false},
	}
	body := []byte(`{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	c := newGrokV156CacheContext(104)

	disabled, err := applyGrokFreeRequestToolCacheRoute(c, body, body, account, "isolated")
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(disabled))

	c.Request.Header.Set(grokClientToolCacheOptInHeader, "true")
	enabled, err := applyGrokFreeRequestToolCacheRoute(c, body, body, account, "isolated")
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(enabled, "tools").Array(), 3)
}

func TestApplyGrokFreeMessagesFunctionToolCacheRouteSkipsPartialBilling(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			grokBillingExtraKey: map[string]any{
				"status_code":        200,
				"monthly_updated_at": "2026-07-16T00:00:00Z",
				"partial":            true,
				"failed_windows":     []string{"weekly"},
			},
		},
	}
	body := []byte(`{"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	patched, err := applyGrokFreeMessagesFunctionToolCacheRoute(body, body, account, "isolated")
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(patched))
}

func TestIsKnownGrokFreeAccountBillingEvidenceIsFailClosed(t *testing.T) {
	newAccount := func(billing *xai.BillingSummary) *Account {
		extra := map[string]any{}
		if billing != nil {
			extra[grokBillingExtraKey] = billing
		}
		return &Account{
			Platform: PlatformGrok,
			Type:     AccountTypeOAuth,
			Extra:    extra,
		}
	}

	require.False(t, isKnownGrokFreeAccount(newAccount(nil)), "missing billing evidence must remain unknown")
	require.False(t, isKnownGrokFreeAccount(newAccount(&xai.BillingSummary{})), "unstamped empty billing must remain unknown")
	require.True(t, isKnownGrokFreeAccount(newAccount(&xai.BillingSummary{
		StatusCode:       http.StatusOK,
		MonthlyUpdatedAt: "2026-07-21T00:00:00Z",
	})), "a complete successful official monthly observation is the Free account shape")
	require.False(t, isKnownGrokFreeAccount(newAccount(&xai.BillingSummary{
		StatusCode:        http.StatusOK,
		MonthlyUpdatedAt:  "2026-07-21T00:00:00Z",
		Plan:              "SuperGrok",
		MonthlyLimitCents: float64PtrForGrokCacheTest(xai.SuperGrokLimitCents),
	})), "explicit paid evidence must override the inferred Free signal")
}

func float64PtrForGrokCacheTest(value int) *float64 {
	converted := float64(value)
	return &converted
}

func TestOpenAIWSLogIdentifiersAreHashed(t *testing.T) {
	const rawID = "resp_sensitive_value"
	require.Equal(t, hashSensitiveValueForLog(rawID), truncateOpenAIWSLogValue(rawID, openAIWSIDValueMaxLen))
	require.NotContains(t, truncateOpenAIWSLogValue(rawID, openAIWSIDValueMaxLen), rawID)
	require.Equal(t, hashSensitiveValueForLog(rawID), shortSessionHash(rawID))

	headers := http.Header{
		"Session_id":      []string{"session_sensitive_value"},
		"Conversation_id": []string{"conversation_sensitive_value"},
		"User-Agent":      []string{"codex_cli_rs/0.1"},
	}
	require.Equal(t, hashSensitiveValueForLog("session_sensitive_value"), openAIWSHeaderValueForLog(headers, "session_id"))
	require.Equal(t, hashSensitiveValueForLog("conversation_sensitive_value"), openAIWSHeaderValueForLog(headers, "conversation_id"))
	require.Equal(t, "codex_cli_rs/0.1", openAIWSHeaderValueForLog(headers, "user-agent"))
}
