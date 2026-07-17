package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type transientAuditBodyReader struct{ step int }

func (r *transientAuditBodyReader) Read(p []byte) (int, error) {
	switch r.step {
	case 0:
		r.step++
		return copy(p, []byte(`{"name":"`)), errors.New("transient read failure")
	case 1:
		r.step++
		return copy(p, []byte(`restored"}`)), io.EOF
	default:
		return 0, io.EOF
	}
}

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

func TestAuditMiddlewareRestoresPrefixAfterAuditReadError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &auditRepoCapture{inserted: make(chan *service.AuditLog, 1)}
	auditService := service.NewAuditLogService(repo, nil)
	auditService.Start()
	t.Cleanup(auditService.Stop)

	router := gin.New()
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	var handlerBody string
	router.POST("/api/v1/admin/prompt-audit/config", func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		handlerBody = string(raw)
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/prompt-audit/config", nil)
	req.Body = io.NopCloser(&transientAuditBodyReader{})
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)
	require.Equal(t, `{"name":"restored"}`, handlerBody)
}

func TestAuditMiddlewareUsesStableAuthActionsAndSecurityIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &auditRepoCapture{inserted: make(chan *service.AuditLog, 4)}
	auditService := service.NewAuditLogService(repo, nil)
	auditService.Start()

	cfg := &config.Config{}
	cfg.SetTrustForwardedIPForAPIKeyACL(true)
	router := gin.New()
	router.Use(SessionBindingContext(cfg))
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	router.POST("/api/v1/auth/login", func(c *gin.Context) {
		c.Status(http.StatusUnauthorized)
	})
	router.POST("/api/v1/auth/refresh", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	router.GET("/api/v1/admin/backups/s3-config", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	loginBody := `{"email":"user@example.com","password":"pw","verify_code":"123456"}`
	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(loginBody))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("CF-Connecting-IP", "1.2.3.4")
	router.ServeHTTP(httptest.NewRecorder(), login)

	refresh := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", strings.NewReader(`{"refresh_token":"secret"}`))
	refresh.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), refresh)

	read := httptest.NewRequest(http.MethodGet, "/api/v1/admin/backups/s3-config", nil)
	read.Header.Set("CF-Connecting-IP", "1.2.3.4")
	router.ServeHTTP(httptest.NewRecorder(), read)
	auditService.Stop()

	entries := make(map[string]*service.AuditLog, 2)
	for len(repo.inserted) > 0 {
		entry := <-repo.inserted
		entries[entry.Action] = entry
	}
	require.Len(t, entries, 2)
	loginEntry := entries[service.AuditActionLogin]
	require.NotNil(t, loginEntry)
	require.Equal(t, "1.2.3.4", loginEntry.ClientIP)
	require.NotContains(t, loginEntry.RequestBody, "pw")
	require.NotContains(t, loginEntry.RequestBody, "123456")
	require.Equal(t, http.StatusUnauthorized, loginEntry.StatusCode)

	readEntry := entries["admin.backups.s3_config.read"]
	require.NotNil(t, readEntry)
	require.Empty(t, readEntry.RequestBody)
}

func TestAuditMiddlewareActionOverrideAndActor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &auditRepoCapture{inserted: make(chan *service.AuditLog, 1)}
	auditService := service.NewAuditLogService(repo, nil)
	auditService.Start()

	router := gin.New()
	router.Use(gin.HandlerFunc(NewAuditLogMiddleware(auditService)))
	router.POST("/api/v1/custom", func(c *gin.Context) {
		SetAuditAction(c, "custom.operation")
		SetAuditActor(c, 42, "actor@example.com")
		c.Status(http.StatusNoContent)
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/custom", nil))
	auditService.Stop()

	entry := <-repo.inserted
	require.Equal(t, "custom.operation", entry.Action)
	require.NotNil(t, entry.ActorUserID)
	require.Equal(t, int64(42), *entry.ActorUserID)
	require.Equal(t, "actor@example.com", entry.ActorEmail)
}
