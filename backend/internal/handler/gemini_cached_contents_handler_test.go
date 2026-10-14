package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type cacheReadFailureRepo struct {
	service.GeminiCachedContentRepository
	err error
}

func (r cacheReadFailureRepo) GetForOwner(context.Context, service.GeminiCacheOwner, string) (*service.GeminiCachedContent, error) {
	return nil, r.err
}

func TestGeminiCacheGetDistinguishesUnavailableAndMissing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"database unavailable", errors.New("private database detail"), 503},
		{"not found", service.ErrGeminiCacheNotFound, 404},
		{"empty repository result", nil, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/cachedContents/nuro_"+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
			c.Params = gin.Params{{Key: "name", Value: "nuro_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
			groupID := int64(3)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 2, UserID: 1, User: &service.User{ID: 1}, GroupID: &groupID, Group: &service.Group{ID: 3, Platform: service.PlatformGemini}})
			cfg := &config.Config{}
			cfg.Gateway.GeminiExplicitCacheEnabled = true
			h := &GatewayHandler{geminiCachedContentService: service.NewGeminiCachedContentService(cacheReadFailureRepo{err: tc.err}, nil, nil, nil, nil, nil, nil, nil, cfg)}
			h.GeminiCachedContents(c)
			require.Equal(t, tc.status, recorder.Code)
			require.NotContains(t, recorder.Body.String(), "private database detail")
		})
	}
}

func TestGeminiCacheErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{service.ErrGeminiCacheInvalidRequest, 400},
		{context.DeadlineExceeded, 503},
		{errors.New("private connection failure"), 503},
		{service.ErrGeminiCacheRejected, 422},
		{service.ErrGeminiCacheConflict, 409},
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		writeCacheManagementError(c, tc.err)
		require.Equal(t, tc.status, recorder.Code)
		require.NotContains(t, recorder.Body.String(), "private connection failure")
	}
}

func TestGeminiCacheReconcileRejectsOversizedChunkedBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body := `{"reason":"valid reason","padding":"` + strings.Repeat("a", 32<<10) + `"}`
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/gemini-cache/reconcile/test/recheck", strings.NewReader(body))
	c.Request.ContentLength = -1
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})
	h := &GatewayHandler{geminiCachedContentService: &service.GeminiCachedContentService{}}
	h.GeminiCacheReconcile(c)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}
