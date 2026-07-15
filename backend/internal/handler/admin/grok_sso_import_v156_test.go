//go:build unit

package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestNormalizeGrokSSOImportTokensDeduplicatesAndSanitizes(t *testing.T) {
	tokens := normalizeGrokSSOImportTokens(
		[]string{"sso=token-1\ntoken-2", "Cookie: sso-rw=token-1", "token-3\r\ntoken-2"},
		"token-0",
	)

	require.Equal(t, []string{"token-0", "token-1", "token-2", "token-3"}, tokens)
}

func TestGrokSSOImportExpiryUsesTokenExpiryWithoutRefreshToken(t *testing.T) {
	tokenExpiry := time.Now().Add(6 * time.Hour).Unix()
	expiresAt, autoPause := grokSSOImportExpiry(nil, nil, &service.GrokTokenInfo{ExpiresAt: tokenExpiry})

	require.NotNil(t, expiresAt)
	require.Equal(t, tokenExpiry, *expiresAt)
	require.NotNil(t, autoPause)
	require.True(t, *autoPause)
}

func TestGrokSSOImportErrorDoesNotExposeRawFailure(t *testing.T) {
	message := grokSSOImportErrorMessage(errors.New("https://private-provider.example token exchange failed"))
	require.Equal(t, "import failed", message)
	require.NotContains(t, message, "private-provider")
}

func TestGrokSSOImportWorkerRecoversPanic(t *testing.T) {
	handler := &GrokOAuthHandler{}
	created, item := handler.safeCreateAccountFromSSOToken(context.Background(), GrokSSOToOAuthRequest{}, "token", 2, 3)

	require.False(t, created)
	require.Equal(t, 2, item.Index)
	require.Equal(t, "internal import failure", item.Error)
}

func TestCloneGrokSSOMapDeepCopiesNestedValues(t *testing.T) {
	source := map[string]any{
		"nested": map[string]any{"value": "original"},
		"items":  []any{map[string]any{"value": "original"}},
	}

	clone := cloneGrokSSOMap(source)
	clone["nested"].(map[string]any)["value"] = "changed"
	clone["items"].([]any)[0].(map[string]any)["value"] = "changed"

	require.Equal(t, "original", source["nested"].(map[string]any)["value"])
	require.Equal(t, "original", source["items"].([]any)[0].(map[string]any)["value"])
}

func TestGrokOAuthReconcileRejectsRefreshWindowBeforeDurationConversion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/v1/admin/grok/oauth/reconcile",
		strings.NewReader(`{"dry_run":true,"apply":false,"refresh_window_seconds":9223372036854775807}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	handler := &GrokOAuthHandler{}

	handler.ReconcileOAuthAccounts(c)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "GROK_OAUTH_RECONCILE_WINDOW_INVALID")
}
