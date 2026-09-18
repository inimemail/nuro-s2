package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenCodeAllIngressProtocols(t *testing.T) {
	for _, ingress := range []string{"responses", "chat", "messages"} {
		for _, protocol := range []string{APIProtocolResponses, APIProtocolAnthropic, APIProtocolChatCompletions} {
			t.Run(ingress+"/"+protocol, func(t *testing.T) {
				model := map[string]string{APIProtocolResponses: "gpt-test", APIProtocolAnthropic: "qwen-test", APIProtocolChatCompletions: "glm-test"}[protocol]
				a := &Account{ID: 91, Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Concurrency: 1,
					Credentials: map[string]any{"api_key": "test", "model_mapping": map[string]any{"alias": model}},
					Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolAdaptive, cnAPIBaseURLsExtraKey: map[string]string{
						APIProtocolResponses: "http://upstream.example/r/v1", APIProtocolAnthropic: "http://upstream.example/a", APIProtocolChatCompletions: "http://upstream.example/c/v1"}}}
				response := successfulRawChatResponse()
				expectedPath := "/c/v1/chat/completions"
				if protocol == APIProtocolResponses {
					expectedPath = "/r/v1/responses"
					response.Body = io.NopCloser(strings.NewReader(`{"id":"resp_ok","object":"response","status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`))
				} else if protocol == APIProtocolAnthropic {
					expectedPath = "/a/v1/messages"
					response.Body = io.NopCloser(strings.NewReader(`{"id":"msg_ok","type":"message","role":"assistant","model":"qwen-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`))
				}
				upstream := &httpUpstreamRecorder{resp: response}
				if protocol == APIProtocolResponses && ingress != "responses" {
					body, _ := io.ReadAll(response.Body)
					response.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":" + string(body) + "}\n\n"))
					response.Header.Set("Content-Type", "text/event-stream")
				}
				if protocol == APIProtocolAnthropic && ingress != "messages" {
					response.Body = io.NopCloser(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ok\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"qwen-test\",\"content\":[],\"usage\":{\"input_tokens\":2}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
					response.Header.Set("Content-Type", "text/event-stream")
				}
				svc := &OpenAIGatewayService{cfg: strongIsolationTestConfig(), httpUpstream: upstream}
				body := []byte(`{"model":"alias","stream":false,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
				if ingress == "responses" {
					body = []byte(`{"model":"alias","stream":false,"input":"hi"}`)
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+ingress, bytes.NewReader(body))
				var result *OpenAIForwardResult
				var err error
				switch ingress {
				case "responses":
					result, err = svc.Forward(context.Background(), c, a, body)
				case "chat":
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, a, body, "", "")
				case "messages":
					result, err = svc.ForwardAsAnthropic(context.Background(), c, a, body, "", "")
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.NotNil(t, upstream.lastReq)
				require.Equal(t, expectedPath, upstream.lastReq.URL.Path)
				require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
			})
		}
	}
}

func TestOpenCodeStoredProtocolAndURLs(t *testing.T) {
	for _, mode := range []string{AccountModeGo, AccountModeZen} {
		for _, protocol := range []string{"", APIProtocolAdaptive, APIProtocolResponses, APIProtocolAnthropic, APIProtocolChatCompletions} {
			extra, creds := normalizeCNProviderStoredConfig(PlatformOpenCodeGo, nil, map[string]any{"account_mode": mode, "api_protocol": protocol})
			a := &Account{Platform: PlatformOpenCodeGo, Credentials: creds, Extra: extra}
			expected := protocol
			if expected == "" {
				expected = APIProtocolAdaptive
			}
			require.Equal(t, expected, a.GetAPIProtocol())
			base := DefaultOpenCodeGoBaseURL
			if mode == AccountModeZen {
				base = DefaultOpenCodeZenBaseURL
			}
			require.Equal(t, base, a.GetCNProtocolBaseURL(APIProtocolResponses))
			require.Equal(t, strings.TrimSuffix(base, "/v1")+"/anthropic", a.GetCNProtocolBaseURL(APIProtocolAnthropic))
		}
	}
}

func TestOpenCodeFixedProtocolUsesCustomBaseURL(t *testing.T) {
	for _, protocol := range []string{APIProtocolResponses, APIProtocolAnthropic, APIProtocolChatCompletions} {
		a := &Account{Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"api_key": "test", "base_url": "https://custom.example/api"},
			Extra:       map[string]any{cnAPIProtocolExtraKey: protocol}}
		require.Equal(t, "https://custom.example/api", a.GetCNProtocolBaseURL(protocol))
	}
}

func TestGeminiSignalResetPreservesUnrelatedErrors(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	svc := &GeminiMessagesCompatService{}
	svc.observeGeminiPayload(c, []byte(`{"promptFeedback":{"blockReason":"SAFETY"}}`), false)
	resetGeminiResponseSignal(c)
	_, exists := GetOpsStreamError(c)
	require.False(t, exists)
	require.Equal(t, false, c.GetBool(OpsClientBusinessLimitedKey))
	svc.markGeminiEmptyResponse(c, nil, true, "")
	MarkOpsStreamError(c, 429, "rate_limit_error", "later error")
	resetGeminiResponseSignal(c)
	event, exists := GetOpsStreamError(c)
	require.True(t, exists)
	require.Equal(t, 429, event.IntendedStatus)
}

func TestGeminiSignalBufferedEmptyChunkIsNotEmptyResponse(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	svc := &GeminiMessagesCompatService{}
	svc.observeGeminiCollectedPayload(c, []byte(`{"candidates":[],"usageMetadata":{"promptTokenCount":1}}`))
	svc.observeGeminiCollectedPayload(c, []byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
	_, exists := GetOpsStreamError(c)
	require.False(t, exists)
	_, _, _, err := collectGeminiSSE(strings.NewReader(": keepalive\n\n"), false, func(body []byte) { svc.observeGeminiCollectedPayload(c, body) })
	require.Error(t, err)
	event, exists := GetOpsStreamError(c)
	require.True(t, exists)
	require.Equal(t, false, *event.Stream)
}

func TestOpenAIFastMissingHTTPWSParity(t *testing.T) {
	for _, action := range []string{BetaPolicyActionPass, BetaPolicyActionFilter, BetaPolicyActionBlock, OpenAIFastPolicyActionForcePriority} {
		svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierMissing, Action: action, Scope: BetaPolicyScopeAll}}})
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
		body := []byte(`{"type":"response.create","model":"gpt-test"}`)
		httpBody, httpErr := svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-test", body)
		wsBody, blocked, wsErr := svc.applyOpenAIFastPolicyToWSResponseCreate(context.Background(), account, "gpt-test", body)
		require.NoError(t, wsErr)
		require.Equal(t, httpBody, wsBody)
		require.Equal(t, httpErr != nil, blocked != nil)
		if action == OpenAIFastPolicyActionForcePriority {
			require.Equal(t, "priority", gjson.GetBytes(wsBody, "service_tier").String())
		}
	}
}

func TestOllamaLabeledResetDoesNotCrossWindows(t *testing.T) {
	d := parseOllamaCloudUsageHTML(`<section><h3>Session usage</h3><p>100%</p></section><section><h3>Weekly usage</h3><p>75%</p><p>Resets in 2 days</p></section>`)
	near := time.Now().Add(48 * time.Hour)
	require.NotNil(t, d.FiveHour)
	require.NotNil(t, d.SevenDay)
	require.Nil(t, d.FiveHour.ResetAt)
	require.WithinDuration(t, near, *d.SevenDay.ResetAt, time.Second)
	d = parseOllamaCloudUsageHTML(`<section>Session usage 100% Resets in 999999999999999999999 days</section>`)
	require.Nil(t, d.FiveHour.ResetAt)
}

func TestGeminiRejectedEnvelopeStillObserved(t *testing.T) {
	svc := &GeminiMessagesCompatService{}
	for _, stream := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		body := `{"error":{"code":503,"status":"UNAVAILABLE","message":"overloaded"}}`
		if stream {
			_, _, _, err := collectGeminiSSE(strings.NewReader("data: "+body+"\n\n"), false, func(b []byte) { svc.observeGeminiPayload(c, b, true) })
			require.Error(t, err)
		} else {
			_, _, err := svc.handleNonStreamingResponse(c, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, "gemini-test")
			require.Error(t, err)
		}
		event, ok := GetOpsStreamError(c)
		require.True(t, ok)
		require.Equal(t, 503, event.IntendedStatus)
		require.NotNil(t, event.Stream)
		require.Equal(t, stream, *event.Stream)
	}
}
