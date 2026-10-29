package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

const malformedToolUsage = `"usageMetadata":{"promptTokenCount":13,"cachedContentTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":2,"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":1}]}`

func TestAntigravityMalformedToolTransformPreservesUsage(t *testing.T) {
	var diagnostics bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&diagnostics)
	defer log.SetOutput(previousOutput)
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprintf("wrapped_%t", wrapped), func(t *testing.T) {
			body := `{"candidates":[{"content":{"parts":[{"text":"PRIVATE_TRANSFORM_CONTENT"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}],` + malformedToolUsage + `}`
			if wrapped {
				body = `{"response":` + body + `}`
			}
			response, usage, err := antigravity.TransformGeminiToClaude([]byte(body), "claude-sonnet-4-5")
			require.ErrorIs(t, err, antigravity.ErrMalformedFunctionCall)
			require.Empty(t, response)
			require.NotNil(t, usage)
			require.Equal(t, 8, usage.InputTokens)
			require.Equal(t, 5, usage.CacheReadInputTokens)
			require.Equal(t, 5, usage.OutputTokens)
			require.Equal(t, 1, usage.ImageOutputTokens)
		})
	}
	log.SetOutput(previousOutput)
	require.NotContains(t, diagnostics.String(), "PRIVATE_TRANSFORM_CONTENT")
}

func TestAntigravityMalformedToolForwardPreservesObservedUsage(t *testing.T) {
	for _, protocol := range []string{"messages", "chat", "responses"} {
		for _, usageFrame := range []string{"prior", "terminal", "both", "prior_null", "prior_empty", "absent"} {
			t.Run(protocol+"/"+usageFrame, func(t *testing.T) {
				prior := `{"response":{"candidates":[{"content":{"parts":[{"text":"PRIVATE_PARTIAL_CONTENT"}]}}]`
				terminal := `{"response":{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]`
				if strings.HasPrefix(usageFrame, "prior") || usageFrame == "both" {
					prior += `,` + malformedToolUsage
				}
				switch usageFrame {
				case "terminal", "both":
					terminal += `,` + malformedToolUsage
				case "prior_null":
					terminal += `,"usageMetadata":null`
				case "prior_empty":
					terminal += `,"usageMetadata":{}`
				}
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("data: " + prior + "}}\n\ndata: " + terminal + "}}\n\n")),
				}}}
				svc := &AntigravityGatewayService{
					settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{}),
					tokenProvider:  &AntigravityTokenProvider{}, httpUpstream: upstream,
				}
				account := &Account{ID: 5, Name: "malformed-usage-test", Platform: PlatformAntigravity,
					Type: AccountTypeOAuth, Status: StatusActive, Concurrency: 1,
					Credentials: map[string]any{"access_token": "test-token", "model_mapping": map[string]any{"claude-sonnet-4-5": "gemini-3-pro-high"}},
				}
				body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
				c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/messages", body)
				var result *ForwardResult
				var err error
				if protocol == "messages" {
					result, err = svc.Forward(context.Background(), c, account, body, false)
				} else {
					compatProtocol := antigravityCompatChatCompletions
					if protocol == "responses" {
						compatProtocol = antigravityCompatResponses
					}
					result, err = svc.consumeAntigravityCompatResponse(context.Background(), c, account,
						&antigravityCompatUpstreamCall{billingModel: "gemini-3-pro-high", request: antigravityCompatRequest{
							protocol: compatProtocol, originalModel: "claude-sonnet-4-5", startTime: time.Now(),
						}}, upstream.responses[0])
				}
				require.Error(t, err)
				require.Equal(t, http.StatusBadGateway, recorder.Code)
				require.NotContains(t, recorder.Body.String(), "PRIVATE_PARTIAL_CONTENT")
				require.Contains(t, recorder.Body.String(), "invalid tool call")
				if protocol == "messages" {
					require.JSONEq(t, `{"type":"error","error":{"type":"malformed_function_call","message":"Upstream returned an invalid tool call"}}`, recorder.Body.String())
				} else {
					require.JSONEq(t, `{"error":{"type":"malformed_function_call","message":"Upstream returned an invalid tool call","param":null,"code":null}}`, recorder.Body.String())
				}
				require.NotNil(t, result, "a failed response must retain its observed usage for the existing settlement path")
				if usageFrame == "absent" {
					require.Equal(t, ClaudeUsage{}, result.Usage, "missing usage must never be estimated")
				} else {
					require.Equal(t, 8, result.Usage.InputTokens)
					require.Equal(t, 5, result.Usage.CacheReadInputTokens)
					require.Equal(t, 5, result.Usage.OutputTokens)
					require.Equal(t, 1, result.Usage.ImageOutputTokens)
				}
				require.Equal(t, "gemini-3-pro-high", result.UpstreamModel)
			})
		}
	}
}
