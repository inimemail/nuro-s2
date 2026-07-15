package service

import (
	"net/http/httptest"
	"testing"

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
