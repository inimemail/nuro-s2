package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const s3ErrorCanary = "https://private-s3.example Authorization=secret <html>cloud error</html>"

func TestBackupS3ConnectionErrorIsNotReturned(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backupService := service.NewBackupService(
		nil,
		&config.Config{},
		nil,
		func(context.Context, *service.BackupS3Config) (service.BackupObjectStore, error) {
			return nil, errors.New(s3ErrorCanary)
		},
		nil,
	)
	handler := NewBackupHandler(backupService, nil)
	router := gin.New()
	router.POST("/test", handler.TestS3Connection)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"bucket":"b","access_key_id":"ak","secret_access_key":"sk"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "S3_CONNECTION_TEST_FAILED")
	require.NotContains(t, recorder.Body.String(), "private-s3.example")
	require.NotContains(t, recorder.Body.String(), "Authorization")
	require.NotContains(t, recorder.Body.String(), "<html>")
}

type failingDataManagementS3Service struct {
	dataManagementService
	result service.DataManagementTestS3Result
	err    error
}

func (s *failingDataManagementS3Service) EnsureAgentEnabled(context.Context) error { return nil }
func (s *failingDataManagementS3Service) ValidateS3(context.Context, service.DataManagementS3Config) (service.DataManagementTestS3Result, error) {
	return s.result, s.err
}

func TestDataManagementS3ConnectionResultIsSanitized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, svc := range []*failingDataManagementS3Service{
		{result: service.DataManagementTestS3Result{OK: false, Message: s3ErrorCanary}},
		{err: errors.New(s3ErrorCanary)},
	} {
		handler := &DataManagementHandler{dataManagementService: svc}
		router := gin.New()
		router.POST("/test", handler.TestS3)

		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(`{"region":"r","bucket":"b"}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, req)

		require.Equal(t, http.StatusOK, recorder.Code)
		require.Contains(t, recorder.Body.String(), "S3_CONNECTION_TEST_FAILED")
		require.NotContains(t, recorder.Body.String(), "private-s3.example")
		require.NotContains(t, recorder.Body.String(), "Authorization")
		require.NotContains(t, recorder.Body.String(), "<html>")
	}
}
