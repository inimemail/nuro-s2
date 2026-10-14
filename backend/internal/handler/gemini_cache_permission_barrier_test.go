package handler

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGeminiCachePermissionWriter(t *testing.T) {
	for _, failure := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		calls := 0
		w := &geminiCachePermissionWriter{ResponseWriter: c.Writer, ctx: context.Background(), wait: func(context.Context) error {
			calls++
			if failure {
				return errors.New("offline")
			}
			return nil
		}}
		w.WriteHeader(http.StatusOK)
		w.WriteHeaderNow()
		_, e := w.WriteString(`{"saved":true}`)
		require.NoError(t, e)
		w.Flush()
		require.Equal(t, 1, calls)
		if failure {
			require.Equal(t, 503, recorder.Code)
			require.NotContains(t, recorder.Body.String(), "saved\":true")
			require.Contains(t, recorder.Body.String(), "Configuration was saved")
		} else {
			require.Equal(t, 200, recorder.Code)
			require.Equal(t, `{"saved":true}`, recorder.Body.String())
		}
	}
}
func TestGeminiCachePermissionMutationScope(t *testing.T) {
	require.True(t, geminiCachePermissionMutation("PUT", "/api/v1/admin/accounts/1"))
	require.True(t, geminiCachePermissionMutation("DELETE", "/api/v1/keys/1"))
	for _, path := range []string{"/api/v1/admin/accounts/1/schedulable", "/api/v1/admin/accounts/batch-delete", "/api/v1/admin/accounts/batch-update-credentials", "/api/v1/admin/accounts/1/refresh"} {
		require.True(t, geminiCachePermissionMutation("POST", path))
	}
	require.True(t, geminiCachePermissionMutation("PUT", "/api/v1/admin/api-keys/1"))
	for _, path := range []string{"/api/v1/admin/subscriptions/1/revoke", "/api/v1/admin/subscriptions/1/restore", "/api/v1/admin/subscriptions/1/extend"} {
		require.True(t, geminiCachePermissionMutation("POST", path))
	}
	for _, path := range []string{"/api/v1/user/password", "/api/v1/user/totp/disable", "/api/v1/user/gemini-cache/1/cachedContents", "/v1beta/models/gemini:generateContent"} {
		require.False(t, geminiCachePermissionMutation("PUT", path))
	}
}
