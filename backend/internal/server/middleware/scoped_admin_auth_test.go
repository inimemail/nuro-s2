//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scopedLegacyUserRepo struct{ stubUserRepo }

func (*scopedLegacyUserRepo) GetFirstAdmin(context.Context) (*service.User, error) {
	return &service.User{ID: 1, Role: service.RoleAdmin, Status: service.StatusActive}, nil
}

func TestScopedAdminLegacyPrefixStillWinsWithoutScopedDatabase(t *testing.T) {
	cfg := &config.Config{}
	cfg.Security.ScopedAdminAPIKeysEnabled = true
	// A nil scoped DB deliberately panics if legacy authentication touches it.
	scoped := service.NewScopedAdminService(cfg, nil, nil, nil, nil)
	secret := service.ScopedAdminKeyPrefix + strings.Repeat("a", 43)
	settings := service.NewSettingService(&bmSettingRepo{values: map[string]string{service.SettingKeyAdminAPIKey: secret}}, cfg)
	users := service.NewUserService(&scopedLegacyUserRepo{}, nil, nil, nil)
	router := gin.New()
	router.Use(adminAuthWithSecurity(nil, users, settings, nil, scoped))
	router.GET("/legacy-only", func(c *gin.Context) {
		_, exists := c.Get(adminaccess.PrincipalContextKey)
		require.False(t, exists)
		require.Equal(t, service.AuditAuthMethodAdminAPIKey, c.GetString("auth_method"))
		c.Status(http.StatusNoContent)
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/legacy-only", nil)
	req.Header.Set("x-api-key", secret)
	router.ServeHTTP(w, req)
	require.Equal(t, 204, w.Code)
}
func TestScopedAdminDisabledGateRejectsWithoutScopedDatabase(t *testing.T) {
	cfg := &config.Config{}
	scoped := service.NewScopedAdminService(cfg, nil, nil, nil, nil)
	settings := service.NewSettingService(&bmSettingRepo{values: map[string]string{}}, cfg)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/v1/admin/accounts", nil)
	require.False(t, validateAdminAPIKey(c, service.ScopedAdminKeyPrefix+strings.Repeat("a", 43), settings, nil, scoped))
	require.Equal(t, 401, c.Writer.Status())
}
func TestScopedAdminEnabledGateDoesNotQueryForJWTOrManagementWS(t *testing.T) {
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "fixture-jwt-secret", ExpireHour: 1}}
	cfg.Security.ScopedAdminAPIKeysEnabled = true
	scoped := service.NewScopedAdminService(cfg, nil, nil, nil, nil)
	auth := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	admin := &service.User{ID: 1, Role: service.RoleAdmin, Status: service.StatusActive, TokenVersion: 2}
	users := service.NewUserService(&stubUserRepo{getByID: func(context.Context, int64) (*service.User, error) { return admin, nil }}, nil, nil, nil)
	token, err := auth.GenerateToken(admin)
	require.NoError(t, err)
	for _, ws := range []bool{false, true} {
		router := gin.New()
		router.Use(adminAuthWithSecurity(auth, users, nil, nil, scoped))
		router.GET("/jwt", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		req := httptest.NewRequest("GET", "/jwt", nil)
		if ws {
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Sec-WebSocket-Protocol", "sub2api-admin, jwt."+token)
			req.Header.Set("x-api-key", "must-not-override-ws")
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		require.Equal(t, 204, w.Code)
	}
}
