package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type blacklistHandlerUserRepo struct {
	service.UserRepository
	actors []int64
}

func (r *blacklistHandlerUserRepo) PreviewRegistrationBlacklist(_ context.Context, actor, _ int64) (*service.RegistrationBlacklistPreview, error) {
	r.actors = append(r.actors, actor)
	return &service.RegistrationBlacklistPreview{Revision: "fixture", Users: []service.RegistrationBlacklistCandidate{}}, nil
}
func (r *blacklistHandlerUserRepo) DisableRegistrationBlacklist(_ context.Context, actor int64, _ service.RegistrationBlacklistDisableRequest) ([]int64, error) {
	r.actors = append(r.actors, actor)
	return []int64{9}, nil
}
func TestRegistrationBlacklistHandlersRequireAdminJWT(t *testing.T) {
	for _, auth := range []struct {
		name, role, method string
		subject            bool
		status             int
	}{
		{"admin_jwt", service.RoleAdmin, service.AuditAuthMethodJWT, true, http.StatusOK},
		{"user_jwt", service.RoleUser, service.AuditAuthMethodJWT, true, http.StatusForbidden},
		{"legacy_global_key", service.RoleAdmin, service.AuditAuthMethodAdminAPIKey, true, http.StatusForbidden},
		{"scoped_key", service.RoleAdmin, "scoped_admin_api_key", true, http.StatusForbidden},
		{"missing_subject", service.RoleAdmin, service.AuditAuthMethodJWT, false, http.StatusForbidden},
		{"missing_method", service.RoleAdmin, "", true, http.StatusForbidden},
	} {
		for _, disable := range []bool{false, true} {
			t.Run(auth.name+map[bool]string{false: "/preview", true: "/disable"}[disable], func(t *testing.T) {
				repo := &blacklistHandlerUserRepo{}
				h := &UserHandler{userService: service.NewUserService(repo, nil, nil, nil)}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"confirm":true,"users":[{"id":9}],"revision":"fixture","batch_id":"fixture"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				if auth.subject {
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 77})
				}
				c.Set(string(middleware.ContextKeyUserRole), auth.role)
				c.Set("auth_method", auth.method)
				if disable {
					h.DisableRegistrationBlacklist(c)
				} else {
					h.PreviewRegistrationBlacklist(c)
				}
				require.Equal(t, auth.status, w.Code, w.Body.String())
				if auth.status == http.StatusOK {
					require.Equal(t, []int64{77}, repo.actors)
				} else {
					require.Empty(t, repo.actors)
				}
			})
		}
	}
}
