package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestImageTaskErrorPayloadDoesNotExposeUpstream(t *testing.T) {
	message := imageTaskErrorMessage([]byte(`<!DOCTYPE html><title>private-provider.example | 502</title>`))
	require.Equal(t, openAIImageTaskSafeErrorMessage, message)
	require.Equal(t, openAIImageTaskSafeErrorMessage, imageTaskErrorMessage([]byte(`{"error":{"message":"AcmeRelay rejected the request"}}`)))

	body := safeOpenAIImageTaskErrorResponse(502, message)
	require.NotContains(t, string(body), "private-provider.example")
	require.NotContains(t, string(body), "DOCTYPE")
}

func TestPublicPersistentImageTaskSanitizesExistingErrorResponse(t *testing.T) {
	item := publicPersistentOpenAIImageTask(&service.OpenAIImageTask{
		ID:           "task_1",
		Status:       service.OpenAIImageTaskStatusError,
		StatusCode:   502,
		ErrorMessage: "private-provider.example returned 502",
		Response:     []byte(`<!DOCTYPE html><title>private-provider.example | 502</title>`),
	})

	require.NotNil(t, item.Error)
	require.Equal(t, openAIImageTaskSafeErrorMessage, item.Error.Message)
	require.NotContains(t, string(item.Response), "private-provider.example")
	require.False(t, strings.Contains(string(item.Response), "DOCTYPE"))
}

func TestWriteImageTaskFinalResponseSanitizesExistingErrorResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h := &OpenAIGatewayHandler{}

	written := h.writeImageTaskFinalResponse(c, &openAIImageTask{
		Status:     openAIImageTaskStatusError,
		StatusCode: http.StatusBadGateway,
		Error:      &openAIImageTaskError{Message: "private-provider.example returned 502"},
		Response:   []byte(`<!DOCTYPE html><title>private-provider.example | 502</title>`),
	})

	require.True(t, written)
	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "private-provider.example")
	require.NotContains(t, recorder.Body.String(), "DOCTYPE")
	require.Contains(t, recorder.Body.String(), openAIImageTaskSafeErrorMessage)
}
