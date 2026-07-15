package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAIImageTaskStatusQueued  = service.OpenAIImageTaskStatusQueued
	openAIImageTaskStatusRunning = service.OpenAIImageTaskStatusRunning
	openAIImageTaskStatusSuccess = service.OpenAIImageTaskStatusSuccess
	openAIImageTaskStatusError   = service.OpenAIImageTaskStatusError

	openAIImageTaskGenerationsEndpoint = "/v1/images/generations"
	openAIImageTaskEditsEndpoint       = "/v1/images/edits"
	openAIImageTaskWorkerHeader        = "X-Sub2API-Image-Task-Worker"
	openAIImageTaskAsyncField          = "taskrun"

	defaultOpenAIImageTaskRetention = 24 * time.Hour
	defaultOpenAIImageTaskTimeout   = 30 * time.Minute
	maxOpenAIImageTaskListItems     = 50
	defaultOpenAIImageTaskWorkers   = 8
	defaultOpenAIImageTaskPoll      = time.Second
	defaultOpenAIImageTaskLock      = 35 * time.Minute
	defaultOpenAIImageTaskMaxQueue  = 100000
	defaultOpenAIImageTaskCleanSize = 1000
)

type openAIImageTask struct {
	ID         string
	OwnerID    string
	Status     string
	Endpoint   string
	Model      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	StatusCode int
	Response   json.RawMessage
	Error      *openAIImageTaskError
}

type openAIImageTaskError struct {
	Message    string `json:"message"`
	StatusCode int    `json:"status_code,omitempty"`
}

type openAIImageTaskPublic struct {
	ID         string                `json:"id"`
	Status     string                `json:"status"`
	Endpoint   string                `json:"endpoint,omitempty"`
	Model      string                `json:"model,omitempty"`
	CreatedAt  string                `json:"created_at"`
	UpdatedAt  string                `json:"updated_at"`
	StatusCode int                   `json:"status_code,omitempty"`
	Response   json.RawMessage       `json:"response,omitempty"`
	Data       json.RawMessage       `json:"data,omitempty"`
	Usage      json.RawMessage       `json:"usage,omitempty"`
	Error      *openAIImageTaskError `json:"error,omitempty"`
}

type openAIImageTaskStore struct {
	mu        sync.Mutex
	tasks     map[string]*openAIImageTask
	retention time.Duration
}

func newOpenAIImageTaskStore(retention time.Duration) *openAIImageTaskStore {
	if retention <= 0 {
		retention = defaultOpenAIImageTaskRetention
	}
	return &openAIImageTaskStore{
		tasks:     make(map[string]*openAIImageTask),
		retention: retention,
	}
}

func openAIImageTaskOwnerID(apiKey *service.APIKey) string {
	if apiKey == nil {
		return "anonymous"
	}
	return fmt.Sprintf("api_key:%d", apiKey.ID)
}

func openAIImageTaskKey(ownerID, taskID string) string {
	return ownerID + ":" + taskID
}

func (s *openAIImageTaskStore) submit(ownerID, taskID, endpoint, model string) (*openAIImageTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(time.Now())
	key := openAIImageTaskKey(ownerID, taskID)
	if task := s.tasks[key]; task != nil {
		return cloneOpenAIImageTask(task), false
	}
	now := time.Now()
	task := &openAIImageTask{
		ID:        taskID,
		OwnerID:   ownerID,
		Status:    openAIImageTaskStatusQueued,
		Endpoint:  endpoint,
		Model:     model,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.tasks[key] = task
	return cloneOpenAIImageTask(task), true
}

func (s *openAIImageTaskStore) markRunning(ownerID, taskID string) {
	s.update(ownerID, taskID, func(task *openAIImageTask) {
		task.Status = openAIImageTaskStatusRunning
		task.Error = nil
	})
}

func (s *openAIImageTaskStore) markSuccess(ownerID, taskID string, statusCode int, response []byte) {
	body := bytes.TrimSpace(response)
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	s.update(ownerID, taskID, func(task *openAIImageTask) {
		task.Status = openAIImageTaskStatusSuccess
		task.StatusCode = statusCode
		task.Response = append(task.Response[:0], body...)
		task.Error = nil
	})
}

func (s *openAIImageTaskStore) markError(ownerID, taskID string, statusCode int, message string, response []byte) {
	message = sanitizeOpenAIImageTaskErrorMessage(message)
	// Error task responses are exposed by the polling API. Never persist the
	// upstream body here: CDN pages and provider diagnostics can disclose the
	// configured upstream host even when the immediate gateway response was safe.
	body := safeOpenAIImageTaskErrorResponse(statusCode, message)
	s.update(ownerID, taskID, func(task *openAIImageTask) {
		task.Status = openAIImageTaskStatusError
		task.StatusCode = statusCode
		task.Response = append(task.Response[:0], body...)
		task.Error = &openAIImageTaskError{Message: message, StatusCode: statusCode}
	})
}

func (s *openAIImageTaskStore) update(ownerID, taskID string, fn func(*openAIImageTask)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[openAIImageTaskKey(ownerID, taskID)]
	if task == nil {
		return
	}
	fn(task)
	task.UpdatedAt = time.Now()
}

func (s *openAIImageTaskStore) list(ownerID string, ids []string) ([]openAIImageTaskPublic, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(time.Now())
	items := make([]openAIImageTaskPublic, 0)
	missing := make([]string, 0)
	if len(ids) > 0 {
		for _, id := range ids {
			task := s.tasks[openAIImageTaskKey(ownerID, id)]
			if task == nil {
				missing = append(missing, id)
				continue
			}
			items = append(items, publicOpenAIImageTask(task))
		}
		return items, missing
	}
	tasks := make([]*openAIImageTask, 0)
	for _, task := range s.tasks {
		if task.OwnerID == ownerID {
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
	})
	if len(tasks) > maxOpenAIImageTaskListItems {
		tasks = tasks[:maxOpenAIImageTaskListItems]
	}
	for _, task := range tasks {
		items = append(items, publicOpenAIImageTask(task))
	}
	return items, nil
}

func (s *openAIImageTaskStore) get(ownerID, taskID string) *openAIImageTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked(time.Now())
	return cloneOpenAIImageTask(s.tasks[openAIImageTaskKey(ownerID, taskID)])
}

func (s *openAIImageTaskStore) wait(ctx context.Context, ownerID, taskID string, timeout time.Duration) *openAIImageTask {
	if timeout <= 0 {
		return s.get(ownerID, taskID)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		task := s.get(ownerID, taskID)
		if task == nil || task.Status == openAIImageTaskStatusSuccess || task.Status == openAIImageTaskStatusError {
			return task
		}
		select {
		case <-ctx.Done():
			return task
		case <-timer.C:
			return task
		case <-ticker.C:
		}
	}
}

func (s *openAIImageTaskStore) cleanupLocked(now time.Time) {
	if s.retention <= 0 {
		return
	}
	for key, task := range s.tasks {
		if task == nil {
			delete(s.tasks, key)
			continue
		}
		switch task.Status {
		case openAIImageTaskStatusSuccess, openAIImageTaskStatusError:
			if now.Sub(task.UpdatedAt) > s.retention {
				delete(s.tasks, key)
			}
		}
	}
}

func cloneOpenAIImageTask(task *openAIImageTask) *openAIImageTask {
	if task == nil {
		return nil
	}
	clone := *task
	if task.Response != nil {
		clone.Response = append(json.RawMessage(nil), task.Response...)
	}
	if task.Error != nil {
		errClone := *task.Error
		clone.Error = &errClone
	}
	return &clone
}

func publicOpenAIImageTask(task *openAIImageTask) openAIImageTaskPublic {
	item := openAIImageTaskPublic{
		ID:         task.ID,
		Status:     task.Status,
		Endpoint:   task.Endpoint,
		Model:      task.Model,
		CreatedAt:  task.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  task.UpdatedAt.Format(time.RFC3339),
		StatusCode: task.StatusCode,
		Error:      task.Error,
	}
	if len(task.Response) > 0 {
		response := task.Response
		if task.Status == openAIImageTaskStatusError {
			message := openAIImageTaskSafeErrorMessage
			if task.Error != nil {
				message = task.Error.Message
			}
			response = safeOpenAIImageTaskErrorResponse(task.StatusCode, message)
		}
		item.Response = append(json.RawMessage(nil), response...)
		if data := gjson.GetBytes(response, "data"); data.Exists() {
			item.Data = json.RawMessage(data.Raw)
		}
		if usage := gjson.GetBytes(response, "usage"); usage.Exists() {
			item.Usage = json.RawMessage(usage.Raw)
		}
	}
	return item
}

func publicPersistentOpenAIImageTask(task *service.OpenAIImageTask) openAIImageTaskPublic {
	if task == nil {
		return openAIImageTaskPublic{}
	}
	errObj := (*openAIImageTaskError)(nil)
	if strings.TrimSpace(task.ErrorMessage) != "" {
		errObj = &openAIImageTaskError{Message: sanitizeOpenAIImageTaskErrorMessage(task.ErrorMessage), StatusCode: task.StatusCode}
	}
	item := openAIImageTaskPublic{
		ID:         task.ID,
		Status:     task.Status,
		Endpoint:   task.Endpoint,
		Model:      task.Model,
		CreatedAt:  task.CreatedAt.Format(time.RFC3339),
		UpdatedAt:  task.UpdatedAt.Format(time.RFC3339),
		StatusCode: task.StatusCode,
		Error:      errObj,
	}
	if len(task.Response) > 0 {
		response := task.Response
		if task.Status == openAIImageTaskStatusError {
			message := openAIImageTaskSafeErrorMessage
			if errObj != nil {
				message = errObj.Message
			}
			response = safeOpenAIImageTaskErrorResponse(task.StatusCode, message)
		}
		item.Response = append(json.RawMessage(nil), response...)
		if data := gjson.GetBytes(response, "data"); data.Exists() {
			item.Data = json.RawMessage(data.Raw)
		}
		if usage := gjson.GetBytes(response, "usage"); usage.Exists() {
			item.Usage = json.RawMessage(usage.Raw)
		}
	}
	return item
}

func (h *OpenAIGatewayHandler) ensureImageTaskStore() *openAIImageTaskStore {
	if h.imageTaskStore == nil {
		h.imageTaskStore = newOpenAIImageTaskStore(defaultOpenAIImageTaskRetention)
	}
	return h.imageTaskStore
}

// CreateImageGenerationTask submits an asynchronous OpenAI Images generation task.
func (h *OpenAIGatewayHandler) CreateImageGenerationTask(c *gin.Context) {
	h.createImageTask(c, openAIImageTaskGenerationsEndpoint)
}

// CreateImageEditTask submits an asynchronous OpenAI Images edit task.
func (h *OpenAIGatewayHandler) CreateImageEditTask(c *gin.Context) {
	h.createImageTask(c, openAIImageTaskEditsEndpoint)
}

// ListImageTasks returns image task status and completed results for the current API key.
func (h *OpenAIGatewayHandler) ListImageTasks(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	ids := parseOpenAIImageTaskIDs(c.Query("ids"))
	if h.imageTaskRepo != nil {
		items, missing, err := h.imageTaskRepo.List(c.Request.Context(), openAIImageTaskOwnerID(apiKey), ids, maxOpenAIImageTaskListItems)
		if err != nil {
			h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to list image tasks")
			return
		}
		publicItems := make([]openAIImageTaskPublic, 0, len(items))
		for _, item := range items {
			publicItems = append(publicItems, publicPersistentOpenAIImageTask(item))
		}
		c.JSON(http.StatusOK, gin.H{
			"object":      "list",
			"data":        publicItems,
			"missing_ids": missing,
		})
		return
	}
	items, missing := h.ensureImageTaskStore().list(openAIImageTaskOwnerID(apiKey), ids)
	c.JSON(http.StatusOK, gin.H{
		"object":      "list",
		"data":        items,
		"missing_ids": missing,
	})
}

func (h *OpenAIGatewayHandler) createImageTask(c *gin.Context, endpoint string) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	if !h.ensureResponsesDependencies(c, requestLogger(c, "handler.openai_gateway.image_tasks")) {
		return
	}

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	taskID, err := extractOpenAIImageTaskID(body, c.GetHeader("Content-Type"))
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if taskID == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "taskid is required for image tasks")
		return
	}

	ownerID := openAIImageTaskOwnerID(apiKey)

	parsed, err := h.parseImageTaskRequestForValidation(c, endpoint, body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if parsed.Stream {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "stream=true is not supported for image tasks")
		return
	}

	taskBody, err := stripOpenAIImageTaskFields(body, c.GetHeader("Content-Type"))
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if h.imageTaskRepo != nil {
		task, created, err := h.submitPersistentImageTask(c, ownerID, taskID, endpoint, parsed.Model, taskBody, apiKey, subject)
		if err != nil {
			h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to submit image task")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		c.JSON(status, publicPersistentOpenAIImageTask(task))
		return
	}
	task, created := h.ensureImageTaskStore().submit(ownerID, taskID, endpoint, parsed.Model)
	if created {
		h.startImageTaskWorker(c, ownerID, taskID, endpoint, taskBody, apiKey, subject)
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	c.JSON(status, publicOpenAIImageTask(task))
}

func (h *OpenAIGatewayHandler) maybeHandleImagesAsTask(
	c *gin.Context,
	endpoint string,
	body []byte,
	parsed *service.OpenAIImagesRequest,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
) bool {
	if parsed == nil {
		return false
	}
	if strings.TrimSpace(c.GetHeader(openAIImageTaskWorkerHeader)) != "" {
		return false
	}
	taskOptions, err := extractOpenAIImageTaskOptions(body, c.GetHeader("Content-Type"))
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true
	}
	if !taskOptions.Async {
		return false
	}
	ownerID := openAIImageTaskOwnerID(apiKey)
	taskID, err := extractOpenAIImageTaskID(body, c.GetHeader("Content-Type"))
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true
	}
	if taskID == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "taskid is required when taskrun is true")
		return true
	}
	taskBody, err := stripOpenAIImageTaskFields(body, c.GetHeader("Content-Type"))
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true
	}
	if h.imageTaskRepo != nil {
		task, created, err := h.submitPersistentImageTask(c, ownerID, taskID, endpoint, parsed.Model, taskBody, apiKey, subject)
		if err != nil {
			h.errorResponse(c, http.StatusInternalServerError, "api_error", "Failed to submit image task")
			return true
		}
		c.Header("Retry-After", "2")
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		c.JSON(status, publicPersistentOpenAIImageTask(task))
		return true
	}
	task, created := h.ensureImageTaskStore().submit(ownerID, taskID, endpoint, parsed.Model)
	if created {
		h.startImageTaskWorker(c, ownerID, taskID, endpoint, taskBody, apiKey, subject)
	}
	c.Header("Retry-After", "2")
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	c.JSON(status, publicOpenAIImageTask(task))
	return true
}

type openAIImageTaskOptions struct {
	Async bool
}

func extractOpenAIImageTaskOptions(body []byte, contentType string) (openAIImageTaskOptions, error) {
	options := openAIImageTaskOptions{}
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err == nil && strings.EqualFold(mediaType, "multipart/form-data") {
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			return options, fmt.Errorf("multipart boundary is required")
		}
		return extractOpenAIImageTaskOptionsFromMultipart(body, boundary)
	}
	if !gjson.ValidBytes(body) {
		return options, fmt.Errorf("failed to parse request body")
	}
	taskRun := gjson.GetBytes(body, openAIImageTaskAsyncField)
	if !taskRun.Exists() {
		return options, nil
	}
	if taskRun.Type != gjson.True {
		return options, fmt.Errorf("taskrun must be true")
	}
	options.Async = true
	return options, nil
}

func extractOpenAIImageTaskOptionsFromMultipart(body []byte, boundary string) (openAIImageTaskOptions, error) {
	options := openAIImageTaskOptions{}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return options, nil
		}
		if err != nil {
			return options, fmt.Errorf("failed to parse multipart request")
		}
		name := strings.TrimSpace(part.FormName())
		if name != openAIImageTaskAsyncField {
			_ = part.Close()
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(part, 4096))
		_ = part.Close()
		if readErr != nil {
			return options, fmt.Errorf("failed to read image task option")
		}
		value := strings.TrimSpace(string(data))
		if !strings.EqualFold(value, "true") {
			return options, fmt.Errorf("taskrun must be true")
		}
		options.Async = true
	}
}

func (h *OpenAIGatewayHandler) writeImageTaskFinalResponse(c *gin.Context, task *openAIImageTask) bool {
	if task == nil {
		return false
	}
	switch task.Status {
	case openAIImageTaskStatusSuccess:
		status := task.StatusCode
		if status < 200 || status >= 300 {
			status = http.StatusOK
		}
		c.Data(status, "application/json; charset=utf-8", task.Response)
		return true
	case openAIImageTaskStatusError:
		status := task.StatusCode
		if status <= 0 {
			status = http.StatusBadGateway
		}
		message := openAIImageTaskSafeErrorMessage
		if task.Error != nil && strings.TrimSpace(task.Error.Message) != "" {
			message = task.Error.Message
		}
		c.Data(status, "application/json; charset=utf-8", safeOpenAIImageTaskErrorResponse(status, message))
		return true
	default:
		return false
	}
}

func (h *OpenAIGatewayHandler) parseImageTaskRequestForValidation(c *gin.Context, endpoint string, body []byte) (*service.OpenAIImagesRequest, error) {
	rec := httptest.NewRecorder()
	taskCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(c.Request.Method, endpoint, bytes.NewReader(body))
	req.Header = c.Request.Header.Clone()
	req.Header.Set("Content-Type", c.GetHeader("Content-Type"))
	taskCtx.Request = req
	return h.gatewayService.ParseOpenAIImagesRequest(taskCtx, body)
}

func (h *OpenAIGatewayHandler) submitPersistentImageTask(
	c *gin.Context,
	ownerID string,
	taskID string,
	endpoint string,
	model string,
	body []byte,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
) (*service.OpenAIImageTask, bool, error) {
	if h == nil || h.imageTaskRepo == nil {
		return nil, false, fmt.Errorf("image task repository is not configured")
	}
	maxQueue := h.imageTaskMaxQueue()
	if maxQueue > 0 {
		count, err := h.imageTaskRepo.CountUnfinished(c.Request.Context())
		if err != nil {
			return nil, false, err
		}
		if count >= int64(maxQueue) {
			return nil, false, fmt.Errorf("image task queue is full")
		}
	}
	task := &service.OpenAIImageTask{
		ID:              taskID,
		OwnerID:         ownerID,
		APIKeyID:        apiKey.ID,
		UserID:          subject.UserID,
		UserConcurrency: subject.Concurrency,
		Endpoint:        endpoint,
		Model:           model,
		RequestBody:     append([]byte(nil), body...),
		RequestHeaders:  sanitizeImageTaskHeaders(c.Request.Header),
	}
	return h.imageTaskRepo.Submit(c.Request.Context(), task)
}

func sanitizeImageTaskHeaders(headers http.Header) map[string][]string {
	out := make(map[string][]string)
	for _, name := range []string{
		"Content-Type",
		"Accept",
		"OpenAI-Beta",
		"OpenAI-Organization",
		"OpenAI-Project",
		"X-Client-Request-ID",
		"X-Request-ID",
		"X-Request-Id",
	} {
		values := headers.Values(name)
		if len(values) == 0 {
			continue
		}
		copied := make([]string, 0, len(values))
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				copied = append(copied, value)
			}
		}
		if len(copied) > 0 {
			out[name] = copied
		}
	}
	return out
}

func (h *OpenAIGatewayHandler) startPersistentImageTaskWorkers() {
	if h == nil || h.imageTaskRepo == nil {
		return
	}
	if h.imageTaskWorkerStop == nil {
		h.imageTaskWorkerStop = make(chan struct{})
	}
	if h.imageTaskWorkerDone == nil {
		h.imageTaskWorkerDone = make(chan struct{})
	}
	workerCount := h.imageTaskWorkerCount()
	h.imageTaskWorkerWG.Add(workerCount + 1)
	for i := 0; i < workerCount; i++ {
		workerID := fmt.Sprintf("image-worker-%d-%d", time.Now().UnixNano(), i)
		go h.persistentImageTaskWorker(workerID)
	}
	go h.persistentImageTaskJanitor()
	go func() {
		h.imageTaskWorkerWG.Wait()
		close(h.imageTaskWorkerDone)
	}()
}

func (h *OpenAIGatewayHandler) StopImageTaskWorkers() {
	if h == nil || h.imageTaskRepo == nil {
		return
	}
	h.imageTaskWorkerStopOnce.Do(func() {
		if h.imageTaskWorkerStop != nil {
			close(h.imageTaskWorkerStop)
		}
	})
	if h.imageTaskWorkerDone != nil {
		<-h.imageTaskWorkerDone
	}
}

func (h *OpenAIGatewayHandler) persistentImageTaskWorker(workerID string) {
	defer h.imageTaskWorkerWG.Done()
	ticker := time.NewTicker(h.imageTaskPollInterval())
	defer ticker.Stop()
	for {
		select {
		case <-h.imageTaskWorkerStop:
			return
		default:
		}

		task, err := h.imageTaskRepo.ClaimNext(context.Background(), workerID, h.imageTaskLockTimeout())
		if err != nil {
			select {
			case <-h.imageTaskWorkerStop:
				return
			case <-ticker.C:
				continue
			}
		}
		if task == nil {
			select {
			case <-h.imageTaskWorkerStop:
				return
			case <-ticker.C:
				continue
			}
		}
		h.executePersistentImageTask(task)
	}
}

func (h *OpenAIGatewayHandler) persistentImageTaskJanitor() {
	defer h.imageTaskWorkerWG.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.imageTaskWorkerStop:
			return
		case <-ticker.C:
			if h.imageTaskRepo == nil {
				continue
			}
			_, _ = h.imageTaskRepo.CleanupFinished(
				context.Background(),
				time.Now().Add(-h.imageTaskRetention()),
				h.imageTaskCleanupBatchSize(),
			)
		}
	}
}

func (h *OpenAIGatewayHandler) executePersistentImageTask(task *service.OpenAIImageTask) {
	if task == nil || h.imageTaskRepo == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			_ = h.imageTaskRepo.MarkError(context.Background(), task.DBID, http.StatusInternalServerError, sanitizeOpenAIImageTaskErrorMessage(fmt.Sprintf("image task panic: %v", r)), nil)
		}
	}()
	ctx, cancel := context.WithTimeout(cloneImageTaskContext(context.Background(), nil, task.ID), h.imageTaskTimeout())
	defer cancel()

	apiKey, err := h.apiKeyService.GetByID(ctx, task.APIKeyID)
	if err != nil {
		_ = h.imageTaskRepo.MarkError(ctx, task.DBID, http.StatusUnauthorized, sanitizeOpenAIImageTaskErrorMessage(err.Error()), nil)
		return
	}
	subject := middleware2.AuthSubject{UserID: task.UserID, Concurrency: task.UserConcurrency}
	if apiKey.User != nil {
		subject.UserID = apiKey.User.ID
		subject.Concurrency = apiKey.User.Concurrency
	}
	subscription, err := h.apiKeyService.GetActiveSubscriptionForAPIKey(ctx, apiKey)
	if err != nil {
		_ = h.imageTaskRepo.MarkError(ctx, task.DBID, http.StatusForbidden, sanitizeOpenAIImageTaskErrorMessage(err.Error()), nil)
		return
	}
	statusCode, responseBody := h.runImageTaskRequest(ctx, task.Endpoint, http.Header(task.RequestHeaders), task.RequestBody, apiKey, subject, subscription)
	if statusCode >= 200 && statusCode < 300 {
		_ = h.imageTaskRepo.MarkSuccess(ctx, task.DBID, statusCode, responseBody)
		return
	}
	_ = h.imageTaskRepo.MarkError(ctx, task.DBID, statusCode, imageTaskErrorMessage(responseBody), safeOpenAIImageTaskErrorResponse(statusCode, imageTaskErrorMessage(responseBody)))
}

func (h *OpenAIGatewayHandler) startImageTaskWorker(
	c *gin.Context,
	ownerID string,
	taskID string,
	endpoint string,
	body []byte,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
) {
	headers := c.Request.Header.Clone()
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	baseCtx := cloneImageTaskContext(c.Request.Context(), apiKey, taskID)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.ensureImageTaskStore().markError(ownerID, taskID, http.StatusInternalServerError, sanitizeOpenAIImageTaskErrorMessage(fmt.Sprintf("image task panic: %v", r)), nil)
			}
		}()
		h.ensureImageTaskStore().markRunning(ownerID, taskID)
		timeout := h.imageTaskTimeout()
		taskCtx, cancel := context.WithTimeout(baseCtx, timeout)
		defer cancel()
		statusCode, responseBody := h.runImageTaskRequest(taskCtx, endpoint, headers, body, apiKey, subject, subscription)
		if statusCode >= 200 && statusCode < 300 {
			h.ensureImageTaskStore().markSuccess(ownerID, taskID, statusCode, responseBody)
			return
		}
		h.ensureImageTaskStore().markError(ownerID, taskID, statusCode, imageTaskErrorMessage(responseBody), nil)
	}()
}

func (h *OpenAIGatewayHandler) imageTaskTimeout() time.Duration {
	timeout := defaultOpenAIImageTaskTimeout
	if h != nil && h.cfg != nil && h.cfg.Gateway.ImageStreamDataIntervalTimeout > 0 {
		configured := time.Duration(h.cfg.Gateway.ImageStreamDataIntervalTimeout)*time.Second + 5*time.Minute
		if configured > timeout {
			timeout = configured
		}
	}
	return timeout
}

func (h *OpenAIGatewayHandler) imageTaskWorkerCount() int {
	if h != nil && h.cfg != nil && h.cfg.Gateway.ImageTasks.WorkerCount > 0 {
		return h.cfg.Gateway.ImageTasks.WorkerCount
	}
	return defaultOpenAIImageTaskWorkers
}

func (h *OpenAIGatewayHandler) imageTaskPollInterval() time.Duration {
	if h != nil && h.cfg != nil && h.cfg.Gateway.ImageTasks.PollIntervalSeconds > 0 {
		return time.Duration(h.cfg.Gateway.ImageTasks.PollIntervalSeconds) * time.Second
	}
	return defaultOpenAIImageTaskPoll
}

func (h *OpenAIGatewayHandler) imageTaskLockTimeout() time.Duration {
	if h != nil && h.cfg != nil && h.cfg.Gateway.ImageTasks.LockTimeoutSeconds > 0 {
		return time.Duration(h.cfg.Gateway.ImageTasks.LockTimeoutSeconds) * time.Second
	}
	timeout := h.imageTaskTimeout() + 5*time.Minute
	if timeout < defaultOpenAIImageTaskLock {
		return defaultOpenAIImageTaskLock
	}
	return timeout
}

func (h *OpenAIGatewayHandler) imageTaskRetention() time.Duration {
	if h != nil && h.cfg != nil && h.cfg.Gateway.ImageTasks.RetentionHours > 0 {
		return time.Duration(h.cfg.Gateway.ImageTasks.RetentionHours) * time.Hour
	}
	return defaultOpenAIImageTaskRetention
}

func (h *OpenAIGatewayHandler) imageTaskMaxQueue() int {
	if h != nil && h.cfg != nil {
		return h.cfg.Gateway.ImageTasks.MaxUnfinishedTasks
	}
	return defaultOpenAIImageTaskMaxQueue
}

func (h *OpenAIGatewayHandler) imageTaskCleanupBatchSize() int {
	if h != nil && h.cfg != nil && h.cfg.Gateway.ImageTasks.CleanupBatchSize > 0 {
		return h.cfg.Gateway.ImageTasks.CleanupBatchSize
	}
	return defaultOpenAIImageTaskCleanSize
}

func cloneImageTaskContext(parent context.Context, apiKey *service.APIKey, taskID string) context.Context {
	ctx := context.Background()
	if parent != nil {
		if requestID, _ := parent.Value(ctxkey.RequestID).(string); strings.TrimSpace(requestID) != "" {
			ctx = context.WithValue(ctx, ctxkey.RequestID, strings.TrimSpace(requestID))
		}
		if clientRequestID, _ := parent.Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(clientRequestID) != "" {
			ctx = context.WithValue(ctx, ctxkey.ClientRequestID, strings.TrimSpace(clientRequestID))
		}
	}
	if clientRequestID, _ := ctx.Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(clientRequestID) == "" {
		ctx = context.WithValue(ctx, ctxkey.ClientRequestID, taskID)
	}
	if apiKey != nil && apiKey.Group != nil && service.IsGroupContextValid(apiKey.Group) {
		ctx = context.WithValue(ctx, ctxkey.Group, apiKey.Group)
	}
	return ctx
}

func (h *OpenAIGatewayHandler) runImageTaskRequest(
	ctx context.Context,
	endpoint string,
	headers http.Header,
	body []byte,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	subscription *service.UserSubscription,
) (int, []byte) {
	rec := httptest.NewRecorder()
	taskCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body)).WithContext(ctx)
	req.Header = headers.Clone()
	req.Header.Set(openAIImageTaskWorkerHeader, "1")
	taskCtx.Request = req
	taskCtx.Set(ctxKeyInboundEndpoint, NormalizeInboundEndpoint(endpoint))
	taskCtx.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	taskCtx.Set(string(middleware2.ContextKeyUser), subject)
	if apiKey != nil && apiKey.User != nil {
		taskCtx.Set(string(middleware2.ContextKeyUserRole), apiKey.User.Role)
	}
	if subscription != nil {
		taskCtx.Set(string(middleware2.ContextKeySubscription), subscription)
	}
	h.Images(taskCtx)
	statusCode := rec.Code
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	return statusCode, rec.Body.Bytes()
}

func extractOpenAIImageTaskID(body []byte, contentType string) (string, error) {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err == nil && strings.EqualFold(mediaType, "multipart/form-data") {
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			return "", fmt.Errorf("multipart boundary is required")
		}
		return extractOpenAIImageTaskIDFromMultipart(body, boundary)
	}
	if !gjson.ValidBytes(body) {
		return "", fmt.Errorf("failed to parse request body")
	}
	if value := strings.TrimSpace(gjson.GetBytes(body, "taskid").String()); value != "" {
		return value, nil
	}
	return "", nil
}

func extractOpenAIImageTaskIDFromMultipart(body []byte, boundary string) (string, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("failed to parse multipart request")
		}
		name := strings.TrimSpace(part.FormName())
		if name != "taskid" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, 4096))
		if err != nil {
			return "", fmt.Errorf("failed to read taskid")
		}
		return strings.TrimSpace(string(data)), nil
	}
}

func stripOpenAIImageTaskFields(body []byte, contentType string) ([]byte, error) {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err == nil && strings.EqualFold(mediaType, "multipart/form-data") {
		boundary := strings.TrimSpace(params["boundary"])
		if boundary == "" {
			return nil, fmt.Errorf("multipart boundary is required")
		}
		return stripOpenAIImageTaskFieldsFromMultipart(body, boundary)
	}
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("failed to parse request body")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse request body")
	}
	delete(payload, "taskid")
	delete(payload, openAIImageTaskAsyncField)
	delete(payload, "stream")
	stripped, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare image task request")
	}
	return stripped, nil
}

func stripOpenAIImageTaskFieldsFromMultipart(body []byte, boundary string) ([]byte, error) {
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	if err := writer.SetBoundary(boundary); err != nil {
		return nil, fmt.Errorf("failed to prepare multipart image task request")
	}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("failed to parse multipart request")
		}
		if isOpenAIImageTaskPrivateField(part.FormName()) {
			_ = part.Close()
			continue
		}
		dst, err := writer.CreatePart(part.Header)
		if err != nil {
			_ = part.Close()
			_ = writer.Close()
			return nil, fmt.Errorf("failed to prepare multipart image task request")
		}
		if _, err := io.Copy(dst, part); err != nil {
			_ = part.Close()
			_ = writer.Close()
			return nil, fmt.Errorf("failed to prepare multipart image task request")
		}
		_ = part.Close()
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to prepare multipart image task request")
	}
	return out.Bytes(), nil
}

func isOpenAIImageTaskPrivateField(name string) bool {
	switch strings.TrimSpace(name) {
	case "taskid", openAIImageTaskAsyncField, "stream":
		return true
	default:
		return false
	}
}

func parseOpenAIImageTaskIDs(value string) []string {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, part := range strings.Split(value, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func imageTaskErrorMessage(_ []byte) string {
	// The message is persisted and returned by the task polling API. Do not
	// preserve provider-controlled text: an unknown provider name can be just
	// as identifying as a hostname, and cannot be reliably detected by a list.
	return openAIImageTaskSafeErrorMessage
}

const openAIImageTaskSafeErrorMessage = "Upstream request failed"

func sanitizeOpenAIImageTaskErrorMessage(_ string) string {
	return openAIImageTaskSafeErrorMessage
}

func safeOpenAIImageTaskErrorResponse(statusCode int, message string) []byte {
	payload := map[string]any{
		"error": map[string]any{
			"type":    "upstream_error",
			"message": sanitizeOpenAIImageTaskErrorMessage(message),
		},
	}
	if statusCode > 0 {
		payload["error"].(map[string]any)["status_code"] = statusCode
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":{"type":"upstream_error","message":"Upstream request failed"}}`)
	}
	return body
}
