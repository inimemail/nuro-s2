package middleware

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type stepUpCheckerStub struct {
	granted    bool
	err        error
	sessionKey string
}

func (s *stepUpCheckerStub) HasStepUpGrant(_ context.Context, _ int64, key string) (bool, error) {
	s.sessionKey = key
	return s.granted, s.err
}

type stepUpUserStub struct {
	user *service.User
	err  error
}

func (s *stepUpUserStub) GetByID(context.Context, int64) (*service.User, error) { return s.user, s.err }

func stepUpTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest("GET", "/sensitive", nil)
	c.Set(string(ContextKeyUser), AuthSubject{UserID: 7})
	c.Set("auth_method", service.AuditAuthMethodJWT)
	c.Set(ContextKeySessionID, "session-family")
	return c, writer
}

func TestEnforceStepUpRequiresSessionGrant(t *testing.T) {
	c, writer := stepUpTestContext()
	checker := &stepUpCheckerStub{}
	users := &stepUpUserStub{user: &service.User{ID: 7, TotpEnabled: true}}
	require.False(t, enforceStepUp(c, checker, users))
	require.Equal(t, 403, writer.Code)
	require.Equal(t, "session-family", checker.sessionKey)
}

func TestEnforceStepUpAllowsGrantedSession(t *testing.T) {
	c, _ := stepUpTestContext()
	checker := &stepUpCheckerStub{granted: true}
	users := &stepUpUserStub{user: &service.User{ID: 7, TotpEnabled: true}}
	require.True(t, enforceStepUp(c, checker, users))
}

func TestEnforceStepUpRejectsAdminAPIKey(t *testing.T) {
	c, writer := stepUpTestContext()
	c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
	require.False(t, enforceStepUp(c, &stepUpCheckerStub{granted: true}, &stepUpUserStub{user: &service.User{TotpEnabled: true}}))
	require.Equal(t, 403, writer.Code)
}

func TestEnforceStepUpHandlesMissingUserWithoutPanic(t *testing.T) {
	c, writer := stepUpTestContext()
	require.False(t, enforceStepUp(c, &stepUpCheckerStub{granted: true}, &stepUpUserStub{}))
	require.Equal(t, 500, writer.Code)
}
