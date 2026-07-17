package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type batchLimitAdminServiceStub struct {
	*stubAdminService
	ids         []int64
	concurrency *int
	rpmLimit    *int
}

func (s *batchLimitAdminServiceStub) BatchUpdateLimits(_ context.Context, ids []int64, concurrency, rpmLimit *int) (int, error) {
	s.ids = append([]int64(nil), ids...)
	s.concurrency = concurrency
	s.rpmLimit = rpmLimit
	return len(ids), nil
}

func TestUserHandlerBatchUpdateLimitsPreservesZeroAndPartialFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := &batchLimitAdminServiceStub{stubAdminService: newStubAdminService()}
	handler := NewUserHandler(adminSvc, nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/batch-limits", bytes.NewBufferString(`{"user_ids":[7,9],"concurrency":0}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.BatchUpdateLimits(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []int64{7, 9}, adminSvc.ids)
	require.NotNil(t, adminSvc.concurrency)
	require.Zero(t, *adminSvc.concurrency)
	require.Nil(t, adminSvc.rpmLimit)
}

func TestUserHandlerBatchUpdateLimitsAllUsers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stub := newStubAdminService()
	stub.users = []service.User{{ID: 3}, {ID: 5}}
	adminSvc := &batchLimitAdminServiceStub{stubAdminService: stub}
	handler := NewUserHandler(adminSvc, nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/batch-limits", bytes.NewBufferString(`{"all":true,"rpm_limit":60}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.BatchUpdateLimits(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []int64{3, 5}, adminSvc.ids)
	require.Equal(t, 60, *adminSvc.rpmLimit)
}

func TestUserHandlerBatchUpdateLimitsRejectsMissingFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewUserHandler(&batchLimitAdminServiceStub{stubAdminService: newStubAdminService()}, nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/batch-limits", bytes.NewBufferString(`{"user_ids":[7]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.BatchUpdateLimits(c)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}
