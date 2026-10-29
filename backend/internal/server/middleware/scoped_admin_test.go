package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	basemiddleware "github.com/Wei-Shaw/sub2api/internal/middleware"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scopedRateStub struct {
	keys   []string
	limits []int
	allow  bool
	err    error
}

func (s *scopedRateStub) Allow(_ context.Context, key string, limit int, window time.Duration) (basemiddleware.AllowResult, error) {
	s.keys = append(s.keys, key)
	s.limits = append(s.limits, limit)
	return basemiddleware.AllowResult{Allowed: s.allow, RetryAfter: window}, s.err
}
func TestScopedAdminRateLimitIdentityAndFailClosed(t *testing.T) {
	stub := &scopedRateStub{allow: true}
	limiter := &PanelRateLimiter{limiter: stub}
	request := func(id int64) *httptest.ResponseRecorder {
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Set(adminaccess.PrincipalContextKey, &service.ScopedAdminPrincipal{Key: service.ScopedAdminKey{ID: id, RPM: 19}})
			c.Set(string(ContextKeyUserRole), service.RoleAdmin)
		})
		router.Use(limiter.Global())
		router.GET("/", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		return w
	}
	require.Equal(t, 204, request(11).Code)
	require.Equal(t, 204, request(12).Code)
	require.Equal(t, []string{"panel:global:admin-key:11", "panel:global:admin-key:12"}, stub.keys)
	require.Equal(t, []int{19, 19}, stub.limits)
	stub.allow = false
	w := request(11)
	require.Equal(t, 429, w.Code)
	require.NotEmpty(t, w.Header().Get("Retry-After"))
	stub.err = errors.New("private redis address")
	w = request(11)
	require.Equal(t, 503, w.Code)
	require.NotContains(t, w.Body.String(), "private redis address")
}
func TestScopedAdminPrincipalCannotSatisfyStepUp(t *testing.T) {
	c, w := stepUpTestContext()
	c.Set(adminaccess.PrincipalContextKey, &service.ScopedAdminPrincipal{Key: service.ScopedAdminKey{ID: 11}})
	c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
	require.False(t, enforceStepUp(c, &stepUpCheckerStub{granted: true}, &stepUpUserStub{user: &service.User{TotpEnabled: true}}))
	require.Equal(t, 403, w.Code)
	require.Contains(t, w.Body.String(), "STEP_UP_ADMIN_API_KEY_FORBIDDEN")
}
