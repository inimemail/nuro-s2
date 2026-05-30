package handler

import (
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
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
	body, err := stripOpenAIImageTaskFields([]byte(`{"client_task_id":"abc","task_id":"old","id":"id1","runapi":false,"runapi_inline_wait_seconds":0,"stream":true,"model":"gpt-image-2","prompt":"draw"}`), "application/json")
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"gpt-image-2","prompt":"draw"}`, string(body))
}

func TestOpenAIImageTaskOptions_JSON(t *testing.T) {
	options, err := extractOpenAIImageTaskOptions([]byte(`{"runapi":false,"runapi_inline_wait_seconds":1.5}`), "application/json")
	require.NoError(t, err)
	require.True(t, options.Async)
	require.Equal(t, 1500*time.Millisecond, options.InlineWait)

	options, err = extractOpenAIImageTaskOptions([]byte(`{"prompt":"draw"}`), "application/json")
	require.NoError(t, err)
	require.False(t, options.Async)
	require.Equal(t, time.Duration(-1), options.InlineWait)
}

func TestStripOpenAIImageTaskFields_RemovesMultipartPrivateFields(t *testing.T) {
	var body strings.Builder
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.SetBoundary("sub2api-boundary"))
	require.NoError(t, writer.WriteField("model", "gpt-image-2"))
	require.NoError(t, writer.WriteField("prompt", "draw"))
	require.NoError(t, writer.WriteField("client_task_id", "task-1"))
	require.NoError(t, writer.WriteField(openAIImageTaskAsyncField, "false"))
	require.NoError(t, writer.WriteField(openAIImageTaskInlineWaitField, "0"))
	require.NoError(t, writer.WriteField("stream", "true"))
	require.NoError(t, writer.Close())

	stripped, err := stripOpenAIImageTaskFields([]byte(body.String()), writer.FormDataContentType())
	require.NoError(t, err)
	require.Contains(t, string(stripped), `name="model"`)
	require.Contains(t, string(stripped), `gpt-image-2`)
	require.Contains(t, string(stripped), `name="prompt"`)
	require.NotContains(t, string(stripped), `name="client_task_id"`)
	require.NotContains(t, string(stripped), `name="`+openAIImageTaskAsyncField+`"`)
	require.NotContains(t, string(stripped), `name="`+openAIImageTaskInlineWaitField+`"`)
	require.NotContains(t, string(stripped), `name="stream"`)
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

func TestMaybeHandleImagesAsTask_RequiresExplicitAsyncFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
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
}

func TestMaybeHandleImagesAsTask_RunAPIHandlesStreamRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	h := &OpenAIGatewayHandler{imageTaskStore: newOpenAIImageTaskStore(time.Hour)}

	ownerID := openAIImageTaskOwnerID(&service.APIKey{ID: 1})
	task, created := h.imageTaskStore.submit(ownerID, "task-1", openAIImageTaskGenerationsEndpoint, "gpt-image-2")
	require.True(t, created)
	require.Equal(t, openAIImageTaskStatusQueued, task.Status)
	h.imageTaskStore.markSuccess(ownerID, "task-1", http.StatusOK, []byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`))

	handled := h.maybeHandleImagesAsTask(
		c,
		openAIImageTaskGenerationsEndpoint,
		[]byte(`{"client_task_id":"task-1","runapi":false,"stream":true,"model":"gpt-image-2","prompt":"draw"}`),
		&service.OpenAIImagesRequest{Model: "gpt-image-2", Stream: true},
		&service.APIKey{ID: 1},
		middleware2.AuthSubject{UserID: 1},
	)
	require.True(t, handled)
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`, rec.Body.String())
}
