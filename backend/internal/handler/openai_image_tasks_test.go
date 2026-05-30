package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIImageTaskStore_SubmitIsIdempotent(t *testing.T) {
	store := newOpenAIImageTaskStore(time.Hour)
	first, created := store.submit("api_key:1", "task-1", "/v1/images/generations", "gpt-image-2")
	require.True(t, created)
	require.Equal(t, openAIImageTaskStatusQueued, first.Status)

	second, created := store.submit("api_key:1", "task-1", "/v1/images/generations", "gpt-image-2")
	require.False(t, created)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, openAIImageTaskStatusQueued, second.Status)
}

func TestOpenAIImageTaskStore_PublicTaskIncludesDataAndUsage(t *testing.T) {
	store := newOpenAIImageTaskStore(time.Hour)
	_, created := store.submit("api_key:1", "task-1", "/v1/images/generations", "gpt-image-2")
	require.True(t, created)

	store.markRunning("api_key:1", "task-1")
	store.markSuccess("api_key:1", "task-1", http.StatusOK, []byte(`{"created":1,"data":[{"url":"https://img.example/1.png"}],"usage":{"total_tokens":12}}`))

	items, missing := store.list("api_key:1", []string{"task-1"})
	require.Empty(t, missing)
	require.Len(t, items, 1)
	require.Equal(t, openAIImageTaskStatusSuccess, items[0].Status)
	require.JSONEq(t, `[{"url":"https://img.example/1.png"}]`, string(items[0].Data))
	require.JSONEq(t, `{"total_tokens":12}`, string(items[0].Usage))

	var response map[string]any
	require.NoError(t, json.Unmarshal(items[0].Response, &response))
	require.Equal(t, float64(1), response["created"])
}

func TestExtractOpenAIImageTaskID_JSONAndMultipart(t *testing.T) {
	taskID, err := extractOpenAIImageTaskID([]byte(`{"client_task_id":"abc","prompt":"draw"}`), "application/json")
	require.NoError(t, err)
	require.Equal(t, "abc", taskID)

	taskID, err = extractOpenAIImageTaskID([]byte(`{"task_id":"fallback","prompt":"draw"}`), "application/json")
	require.NoError(t, err)
	require.Equal(t, "fallback", taskID)
}

func TestStripOpenAIImageTaskFields_RemovesTaskOnlyFields(t *testing.T) {
	body, err := stripOpenAIImageTaskFields([]byte(`{"client_task_id":"abc","task_id":"old","id":"id1","model":"gpt-image-2","prompt":"draw"}`), "application/json")
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"gpt-image-2","prompt":"draw"}`, string(body))
}

func TestAutoOpenAIImageTaskID_IsStableAndScoped(t *testing.T) {
	first := autoOpenAIImageTaskID("api_key:1", "/v1/images/generations", []byte(`{"prompt":"draw"}`))
	second := autoOpenAIImageTaskID("api_key:1", "/v1/images/generations", []byte(`{"prompt":"draw"}`))
	otherOwner := autoOpenAIImageTaskID("api_key:2", "/v1/images/generations", []byte(`{"prompt":"draw"}`))
	require.Equal(t, first, second)
	require.NotEqual(t, first, otherOwner)
	require.Contains(t, first, "auto_")
}

func TestMaybeHandleImagesAsTask_SkipsWorkerAndStreamRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	c.Request.Header.Set(openAIImageTaskWorkerHeader, "1")
	h := &OpenAIGatewayHandler{}
	handled := h.maybeHandleImagesAsTask(
		c,
		openAIImageTaskGenerationsEndpoint,
		[]byte(`{"model":"gpt-image-2","prompt":"draw"}`),
		&service.OpenAIImagesRequest{Model: "gpt-image-2"},
		&service.APIKey{ID: 1},
		middleware2.AuthSubject{UserID: 1},
	)
	require.False(t, handled)

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	handled = h.maybeHandleImagesAsTask(
		c,
		openAIImageTaskGenerationsEndpoint,
		[]byte(`{"model":"gpt-image-2","prompt":"draw","stream":true}`),
		&service.OpenAIImagesRequest{Model: "gpt-image-2", Stream: true},
		&service.APIKey{ID: 1},
		middleware2.AuthSubject{UserID: 1},
	)
	require.False(t, handled)
}
