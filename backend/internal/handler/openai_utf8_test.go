package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResponsesRejectsInvalidUTF8BeforeAdmission(t *testing.T) {
	for _, invalid := range [][]byte{{0xff}, {0xe4, 0xb8}, {0xc0, 0xaf}} {
		t.Run(string(invalid), func(t *testing.T) {
			body := append([]byte(`{"model":"gpt-6.1-sol","input":"`), invalid...)
			body = append(body, []byte(`"}`)...)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1, User: &service.User{ID: 1}})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 1})
			cache := &concurrencyCacheMock{
				acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
					t.Error("invalid UTF-8 must not acquire a user slot")
					return false, nil
				},
				acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
					t.Error("invalid UTF-8 must not acquire an account slot")
					return false, nil
				},
			}
			h := newOpenAIHandlerForPreviousResponseIDValidation(t, cache)
			h.Responses(c)
			require.Equal(t, http.StatusBadRequest, w.Code)
			require.Contains(t, w.Body.String(), "Request body must be valid UTF-8")
		})
	}
}
