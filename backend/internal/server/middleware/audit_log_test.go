package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type auditRepoCapture struct{ inserted chan *service.AuditLog }

func (r *auditRepoCapture) BatchInsert(_ context.Context, entries []*service.AuditLog) (int64, error) {
	for _, entry := range entries {
		r.inserted <- entry
	}
	return int64(len(entries)), nil
}
func (r *auditRepoCapture) Insert(context.Context, *service.AuditLog) error { return nil }
func (r *auditRepoCapture) List(context.Context, *service.AuditLogFilter) (*service.AuditLogList, error) {
	return nil, nil
}
func (r *auditRepoCapture) GetByID(context.Context, int64) (*service.AuditLog, error) {
	return nil, nil
}
func (r *auditRepoCapture) Count(context.Context) (int64, error) { return 0, nil }
func (r *auditRepoCapture) TruncateAll(context.Context) error    { return nil }
func (r *auditRepoCapture) DeleteBefore(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func TestAuditMiddlewareRestoresAndRedactsJSONBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &auditRepoCapture{inserted: make(chan *service.AuditLog, 1)}
	auditService := service.NewAuditLogService(repo, nil)
	auditService.Start()
	t.Cleanup(auditService.Stop)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyUser), AuthSubject{UserID: 7})
		c.Set(string(ContextKeyUserRole), service.RoleAdmin)
		c.Set(ContextKeyAuthEmail, "admin@example.com")
		c.Set("auth_method", service.AuditAuthMethodJWT)
		c.Next()
	})
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	var handlerBody string
	router.POST("/api/v1/admin/accounts", func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		handlerBody = string(raw)
		c.Status(http.StatusCreated)
	})

	body := `{"name":"primary","api_key":"raw-secret-value"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer abcdefghijklmnopqrstuvwxyz")
	writer := httptest.NewRecorder()
	router.ServeHTTP(writer, req)

	require.Equal(t, http.StatusCreated, writer.Code)
	require.Equal(t, body, handlerBody)
	select {
	case entry := <-repo.inserted:
		require.Equal(t, "admin.accounts.create", entry.Action)
		require.Equal(t, "admin@example.com", entry.ActorEmail)
		require.NotContains(t, entry.RequestBody, "raw-secret-value")
		require.Contains(t, entry.RequestBody, `"api_key":"***"`)
		require.NotContains(t, entry.CredentialMasked, "abcdefghijklmnopqrstuvwxyz")
	case <-time.After(2 * time.Second):
		t.Fatal("audit entry was not flushed")
	}
}
