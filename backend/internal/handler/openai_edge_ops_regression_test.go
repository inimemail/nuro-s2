package handler

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEdgeForwardedFailureReachesOps(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		callbackOwned bool
		wantLogs      int64
	}{
		{"failed_event", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_test\",\"status\":\"failed\",\"error\":{\"type\":\"upstream_error\",\"code\":\"server_error\",\"message\":\"Upstream request failed\"}}}\n\n", false, 1},
		{"unexpected_eof", "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n", false, 1},
		{"callback_owns_failure", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", true, 0},
		{"local_eof_still_recorded", "", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetOpsErrorLoggerStateForTest(t)
			t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
			opsErrorLogOnce.Do(func() {})
			opsErrorLogQueue = make(chan opsErrorLogJob, 4)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			r := gin.New()
			r.Use(OpsErrorLoggerMiddleware(ops))
			r.POST("/v1/responses", func(c *gin.Context) {
				c.Set("edge_ops_callback_owned", tc.callbackOwned)
				setOpsRequestContext(c, "codex-auto-review", true)
				c.Header("Content-Type", "text/event-stream")
				c.Status(http.StatusOK)
				c.Writer.Flush()
				copyOpenAIEdgeResponseBody(c, strings.NewReader(tc.body), true)
			})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Contains(t, rec.Body.String(), "response.failed")
			require.Equal(t, tc.wantLogs, OpsErrorLogEnqueuedTotal())
			if tc.wantLogs > 0 {
				job := <-opsErrorLogQueue
				require.Equal(t, http.StatusBadGateway, job.entry.StatusCode,
					"the default Ops query filters out status < 400 even for failed SSE")
			}
		})
	}
}

func TestOpenAIEdgeAbortKeepsNonJSONUpstreamStatus(t *testing.T) {
	resetOpsErrorLoggerStateForTest(t)
	t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
	opsErrorLogOnce.Do(func() {})
	opsErrorLogQueue = make(chan opsErrorLogJob, 4)
	h := &OpenAIGatewayHandler{opsService: service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)}
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-html", leaseID: "lease-html", inboundEndpoint: "/v1/responses",
		account: &service.Account{ID: 12}, apiKey: &service.APIKey{ID: 34},
		lastFailureStatus: 429, lastFailureRequestID: "upstream-html",
		lastFailureDiagnostic: edgeOpsDiagnosticFromPayload([]byte("<html>private rate limit page</html>")),
	}
	h.recordOpenAIEdgeFailure(lease, "abort", "retry_exhausted", "retry_failure_already_recorded:exhausted", "", "", 0)
	require.Equal(t, int64(1), OpsErrorLogEnqueuedTotal())
	job := <-opsErrorLogQueue
	require.NotNil(t, job.entry.UpstreamStatusCode)
	require.Equal(t, 429, *job.entry.UpstreamStatusCode)
	require.Equal(t, "upstream-html", job.entry.UpstreamErrors[0].UpstreamRequestID)
	require.Equal(t, "rate_limit_error", job.entry.ErrorType)
	require.Equal(t, "P1", job.entry.Severity)
	require.Equal(t, "provider", job.entry.ErrorOwner)
}

func TestOpenAIEdgeOpsRedisStallDoesNotUseGlobalSocketTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })
	go func() { _, _ = io.Copy(io.Discard, serverConn) }()
	rdb := redis.NewClient(&redis.Options{
		Dialer:      func(context.Context, string, string) (net.Conn, error) { return clientConn, nil },
		ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		MaxRetries: -1, PoolSize: 1, DisableIdentity: true,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	h := &OpenAIGatewayHandler{redisClient: rdb}
	started := time.Now()
	require.True(t, h.claimEdgeOpsFailure("stalled-redis"))
	require.Less(t, time.Since(started), 750*time.Millisecond,
		"diagnostic dedupe must not hold settlement for the global Redis read timeout")
	require.Equal(t, 2*time.Second, rdb.Options().ReadTimeout, "other Redis callers must stay unchanged")
}

func TestOpenAIEdgeActualCallbacksObserveFailuresOnly(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, fields string
		want                   int64
	}{
		{"zero_usage_failure", "complete", `"success":false,"terminal_event_type":"response.failed","upstream_status_code":200`, 1},
		{"completed", "complete", `"success":true,"terminal_event_type":"response.completed"`, 0},
		{"inconsistent_success", "complete", `"success":true,"terminal_event_type":"response.failed"`, 1},
		{"cancelled", "complete", `"success":false,"client_disconnected":true`, 0},
		{"neutral_terminal", "complete", `"success":false,"terminal_event_type":" RESPONSE.INCOMPLETE "`, 0},
		{"abort", "abort", `"relay_attempted":true,"failure_class":"upstream_error","reason":"retry_failure_already_recorded:upstream_failed"`, 1},
		{"abort_cancelled", "abort", `"client_disconnected":true`, 0},
		{"go_fallback", "abort", `"fallback_to_go":true`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetOpsErrorLoggerStateForTest(t)
			t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
			opsErrorLogOnce.Do(func() {})
			opsErrorLogQueue = make(chan opsErrorLogJob, 4)
			lease := &openAIEdgeLease{
				edgeRequestID: "edge-callback", leaseID: "lease-callback", inboundEndpoint: "/v1/responses",
				account: &service.Account{ID: 12, Platform: service.PlatformOpenAI}, apiKey: &service.APIKey{ID: 34},
			}
			h := &OpenAIGatewayHandler{
				cfg: &config.Config{}, opsService: service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil),
				openAIEdgeLeases:         map[string]*openAIEdgeLease{lease.leaseID: lease},
				openAIEdgeLeaseByRequest: map[string]string{lease.edgeRequestID: lease.leaseID},
			}
			h.cfg.Gateway.OpenAIEdgeRS.InternalAPIEnabled = true
			h.cfg.Gateway.OpenAIEdgeRS.InternalSecret = "edge-secret"
			body := `{"edge_request_id":"edge-callback","lease_id":"lease-callback","account_id":12,` + tc.fields + `}`
			c, w := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/"+tc.endpoint, body, "edge-secret")
			if tc.endpoint == "complete" {
				h.OpenAIEdgeComplete(c)
			} else {
				h.OpenAIEdgeAbort(c)
			}
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, tc.want, OpsErrorLogEnqueuedTotal())
		})
	}
}

func TestOpenAIEdgeRetryDoesNotAttributePreviousDiagnosticToNewAttempt(t *testing.T) {
	lease := &openAIEdgeLease{lastFailureDiagnostic: `{"code":"model_not_found"}`, lastFailureStatus: 404, lastFailureRequestID: "old-request", lastFailureResponseID: "old-response"}
	applyOpenAIEdgePreparedRetryPlan(lease, &openAIEdgePreparedRetryPlan{account: &service.Account{ID: 2}})
	require.Empty(t, lease.lastFailureDiagnostic)
	require.Zero(t, lease.lastFailureStatus)
	require.Empty(t, lease.lastFailureRequestID)
	require.Empty(t, lease.lastFailureResponseID)
}

func TestOpenAIEdgeSuccessfulPlaceholderStreamRemainsUnchanged(t *testing.T) {
	resetOpsErrorLoggerStateForTest(t)
	t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
	opsErrorLogOnce.Do(func() {})
	opsErrorLogQueue = make(chan opsErrorLogJob, 4)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	body := ":\n\nevent: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\nevent: response.transport_progress.delta\ndata: {\"type\":\"response.transport_progress.delta\",\"delta\":\"\"}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	r := gin.New()
	r.Use(OpsErrorLoggerMiddleware(ops))
	r.POST("/v1/responses", func(c *gin.Context) {
		c.Status(http.StatusOK)
		copyOpenAIEdgeResponseBody(c, strings.NewReader(body), true)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	require.Equal(t, body, rec.Body.String())
	require.Zero(t, OpsErrorLogEnqueuedTotal())
}

func TestOpenAIEdgeCallbackFailureDeduplicatesAndPreservesDiagnostic(t *testing.T) {
	resetOpsErrorLoggerStateForTest(t)
	t.Cleanup(func() { resetOpsErrorLoggerStateForTest(t) })
	opsErrorLogOnce.Do(func() {})
	opsErrorLogQueue = make(chan opsErrorLogJob, 4)
	h := &OpenAIGatewayHandler{opsService: service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)}
	lease := &openAIEdgeLease{
		edgeRequestID: "edge-test", leaseID: "lease-test", requestModel: "codex-auto-review",
		account: &service.Account{ID: 12, Name: "test", Platform: service.PlatformOpenAI},
		apiKey:  &service.APIKey{ID: 34}, inboundEndpoint: "/v1/responses",
	}
	diagnostic := edgeOpsDiagnosticFromPayload([]byte(`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"model_not_found","message":"private secret https://provider.example"}}}`))
	h.recordOpenAIEdgeFailure(lease, "complete", "upstream_error", diagnostic, "upstream-test", "resp_test", 200)
	h.recordOpenAIEdgeFailure(lease, "complete", "upstream_error", diagnostic, "upstream-test", "resp_test", 200)
	require.Equal(t, int64(1), OpsErrorLogEnqueuedTotal())
	job := <-opsErrorLogQueue
	require.Equal(t, "codex-auto-review", job.entry.Model)
	require.Equal(t, int64(12), *job.entry.AccountID)
	require.Equal(t, 200, *job.entry.UpstreamStatusCode)
	require.Equal(t, http.StatusBadRequest, job.entry.StatusCode,
		"retain raw upstream 200 separately while exposing model_not_found in the error list")
	require.Contains(t, job.entry.UpstreamErrors[0].Detail, "model_not_found")
	require.Contains(t, job.entry.UpstreamErrors[0].Detail, "resp_test")
	require.NotContains(t, job.entry.UpstreamErrors[0].Detail, "secret")
	require.NotContains(t, job.entry.UpstreamErrors[0].Detail, "provider.example")
	require.Equal(t, "P3", job.entry.Severity)
	require.Equal(t, "upstream_http", job.entry.ErrorSource)
}
