package service

import (
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResponsesCreatedAtStamper_StampsMissingAndKeepsStreamStable(t *testing.T) {
	var stamper responsesCreatedAtStamper

	created, changed := stamper.Stamp([]byte(`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`), "response.created")
	require.True(t, changed)
	first := gjson.GetBytes(created, "response.created_at")
	require.Equal(t, gjson.Number, first.Type)
	require.Positive(t, first.Int())

	completed, changed := stamper.Stamp([]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`), "response.completed")
	require.True(t, changed)
	require.Equal(t, first.Int(), gjson.GetBytes(completed, "response.created_at").Int(), "同一条流的 created_at 必须一致")
}

func TestResponsesCreatedAtStamper_PreservesUpstreamValue(t *testing.T) {
	var stamper responsesCreatedAtStamper
	upstream := []byte(`{"type":"response.created","response":{"id":"resp_1","created_at":1700000000}}`)

	out, changed := stamper.Stamp(upstream, "response.created")
	require.False(t, changed)
	require.Equal(t, string(upstream), string(out), "上游已带 created_at 时字节不变")

	completed, changed := stamper.Stamp([]byte(`{"type":"response.completed","response":{"id":"resp_1"}}`), "response.completed")
	require.True(t, changed)
	require.EqualValues(t, 1700000000, gjson.GetBytes(completed, "response.created_at").Int(), "后续缺失时沿用上游值")
	require.EqualValues(t, 1700000000, stamper.Value())
}

func TestResponsesCreatedAtStamper_SyntheticFailureKeepsTimestampAndNextSequence(t *testing.T) {
	var s responsesCreatedAtStamper
	s.Observe([]byte(`{"type":"response.created","sequence_number":0,"response":{"created_at":1700000000}}`), "response.created")
	s.Observe([]byte(`{"type":"response.output_text.delta","sequence_number":9,"delta":"hi"}`), "response.output_text.delta")
	failed := s.Failed(nil, true)
	require.EqualValues(t, 1700000000, gjson.GetBytes(failed, "response.created_at").Int())
	require.EqualValues(t, 10, gjson.GetBytes(failed, "sequence_number").Int())
	source := []byte(`{"type":"error","sequence_number":10,"message":"private upstream diagnostic"}`)
	failed = s.Failed(source, true)
	require.EqualValues(t, 10, gjson.GetBytes(failed, "sequence_number").Int())
	require.NotContains(t, string(failed), "private upstream diagnostic")
	require.Equal(t, "response", gjson.GetBytes(failed, "response.object").String())
}

func TestResponsesCreatedAt_NonStreamingOnlyTouchesMissingResponseTimestamp(t *testing.T) {
	for _, original := range []string{`{"object":"response","created_at":1700000000,"usage":{"input_tokens":10}}`, `{"object":"chat.completion","choices":[]}`, `{"object":"response","created_at":"vendor-value"}`} {
		require.Equal(t, original, string(ensureOpenAIResponseObjectCreatedAt([]byte(original))))
	}
	patched := ensureOpenAIResponseObjectCreatedAt([]byte(`{"object":"response","created_at":null,"usage":{"input_tokens":10}}`))
	require.Positive(t, gjson.GetBytes(patched, "created_at").Int())
	require.EqualValues(t, 10, gjson.GetBytes(patched, "usage.input_tokens").Int())
}

func TestResponsesCreatedAt_BareErrorWithoutSequenceUsesNextStreamSlot(t *testing.T) {
	var tracker responsesCreatedAtStamper
	tracker.Observe([]byte(`{"type":"response.created","sequence_number":3,"response":{"created_at":1700000000}}`), "response.created")
	tracker.Observe([]byte(`{"type":"response.output_text.delta","sequence_number":9,"delta":"ok"}`), "response.output_text.delta")
	source := []byte(`{"type":"error","message":"private diagnostic"}`)
	tracker.Observe(source, "error")
	sanitized := safeOpenAIResponsesFailedPayload(source, true)
	out := tracker.StampForClient(sanitized, "response.failed", "error", source)
	require.EqualValues(t, 10, gjson.GetBytes(out, "sequence_number").Int())
	require.EqualValues(t, 1700000000, gjson.GetBytes(out, "response.created_at").Int())
	require.NotContains(t, string(out), "private diagnostic")
}

func TestResponsesCreatedAt_ActualSSEForwardersKeepUsageAndTimestamps(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough], func(t *testing.T) {
			stream := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_created","created_at":1700000000}}` + "\n\n" +
				"data: " + `{"type":"response.output_text.delta","sequence_number":1,"delta":"ok"}` + "\n\n" +
				"data: " + `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_created","object":"response","status":"completed","output":[],"usage":{"input_tokens":123,"output_tokens":7}}}` + "\n\n"
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, toolCorrector: NewCodexToolCorrector()}
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
			if passthrough {
				result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6", "gpt-5.6")
				require.NoError(t, err)
				require.Equal(t, 123, result.usage.InputTokens)
				require.Equal(t, 7, result.usage.OutputTokens)
			} else {
				result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6", "gpt-5.6")
				require.NoError(t, err)
				require.Equal(t, 123, result.usage.InputTokens)
				require.Equal(t, 7, result.usage.OutputTokens)
			}
			seen := false
			forEachOpenAISSEDataPayload(rec.Body.String(), func(data []byte) {
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					seen = true
					require.EqualValues(t, 1700000000, gjson.GetBytes(data, "response.created_at").Int())
					require.EqualValues(t, 123, gjson.GetBytes(data, "response.usage.input_tokens").Int())
				}
			})
			require.True(t, seen)
		})
	}
}

func TestResponsesCreatedAt_ActualSSEBareErrorKeepsFailureSequence(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "passthrough"}[passthrough], func(t *testing.T) {
			stream := "data: " + `{"type":"response.created","sequence_number":0,"response":{"id":"resp_created","created_at":1700000000}}` + "\n\n" +
				"data: " + `{"type":"response.output_text.delta","sequence_number":8,"delta":"ok"}` + "\n\n" +
				"data: " + `{"type":"error","error":{"message":"private diagnostic"}}` + "\n\n"
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, toolCorrector: NewCodexToolCorrector()}
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
			if passthrough {
				_, _ = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6", "gpt-5.6")
			} else {
				_, _ = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "gpt-5.6", "gpt-5.6")
			}
			failed := 0
			forEachOpenAISSEDataPayload(rec.Body.String(), func(data []byte) {
				if gjson.GetBytes(data, "type").String() == "response.failed" {
					failed++
					require.EqualValues(t, 9, gjson.GetBytes(data, "sequence_number").Int())
					require.EqualValues(t, 1700000000, gjson.GetBytes(data, "response.created_at").Int())
				}
			})
			require.Equal(t, 1, failed)
			require.NotContains(t, rec.Body.String(), "private diagnostic")
		})
	}
}
