package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestImageTaskValidationUsesOriginalBudget(t *testing.T) {
	budget := httputil.NewBodyBudget(32)
	scope := budget.Scope()
	defer scope.Release()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := `{"model":"gpt-image-2.6-flare","prompt":"fixture"}`
	c.Request = httptest.NewRequest("POST", "/v1/images/tasks", strings.NewReader(body)).WithContext(httputil.WithBodyReservation(t.Context(), scope))
	c.Request.Header.Set("Content-Type", "application/json")
	h := &OpenAIGatewayHandler{gatewayService: &service.OpenAIGatewayService{}}
	_, err := h.parseImageTaskRequestForValidation(c, "/v1/images/generations", []byte(body))
	require.ErrorIs(t, err, httputil.ErrBodyBudgetExceeded)
	require.True(t, h.writeImageTaskBudgetError(c, err))
	require.Equal(t, 503, rec.Code)
	require.NotContains(t, rec.Body.String(), "fixture")
	require.Zero(t, budget.Used())
}
