//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMessagesEffortPolicyForwardAndAccountingAgree(t *testing.T) {
	for _, tc := range []struct {
		name, model, mapped, fields, from, to, max, action, want string
		status                                                   int
	}{
		{name: "scoped alias downgrade", model: "my-sol", mapped: "gpt-6.1-sol", fields: `,"output_config":{"effort":"max"}`, from: "max", to: "high", want: "high"},
		{name: "opaque upstream alias preserves source effort mapping", model: "gpt-6.1-sol", mapped: "vendor/custom-model", fields: `,"output_config":{"effort":"max"}`, from: "max", to: "high", want: "high"},
		{name: "terra keeps max", model: "gpt-6.1-terra", fields: `,"output_config":{"effort":"max"}`, want: "max"},
		{name: "none mapping before sol validation", model: "my-sol", mapped: "gpt-6.1-sol", fields: `,"thinking":{"type":"disabled"},"temperature":0.2,"top_p":0.9`, from: "none", to: "low", want: "low"},
		{name: "default is not capped", model: "gpt-6.1-sol", max: "low", action: "deny", want: "medium"},
		{name: "cap after mapping", model: "gpt-6.1-sol", fields: `,"output_config":{"effort":"high"}`, from: "high", to: "max", max: "medium", want: "medium"},
		{name: "deny", model: "gpt-6.1-sol", fields: `,"output_config":{"effort":"max"}`, max: "medium", action: "deny", status: 403},
		{name: "unmapped disabled rejected", model: "gpt-6.1-sol", fields: `,"thinking":{"type":"disabled"}`, status: 400},
		{name: "opaque upstream cannot bypass disabled sol validation", model: "gpt-6.1-sol", mapped: "vendor/custom-model", fields: `,"thinking":{"type":"disabled"}`, status: 400},
		{name: "opaque upstream applies disabled sol mapping", model: "gpt-6.1-sol", mapped: "vendor/custom-model", fields: `,"thinking":{"type":"disabled"},"temperature":0.2,"top_p":0.9`, from: "none", to: "low", want: "low"},
		{name: "opaque upstream preserves disabled terra", model: "gpt-6.1-terra", mapped: "vendor/custom-model", fields: `,"thinking":{"type":"disabled"}`, want: "none"},
		{name: "generic disabled keeps bridge default", model: "vendor/custom-model", fields: `,"thinking":{"type":"disabled"}`, want: "medium"},
		{name: "disabled takes precedence over explicit max", model: "gpt-6.1-sol", fields: `,"thinking":{"type":"disabled"},"output_config":{"effort":"max"}`, status: 400},
		{name: "disabled with explicit max maps from none", model: "gpt-6.1-sol", mapped: "vendor/custom-model", fields: `,"thinking":{"type":"disabled"},"output_config":{"effort":"max"},"temperature":0.2,"top_p":0.9`, from: "none", to: "low", want: "low"},
		{name: "terra disabled overrides explicit max", model: "gpt-6.1-terra", fields: `,"thinking":{"type":"disabled"},"output_config":{"effort":"max"}`, want: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","max_tokens":128,"messages":[{"role":"user","content":"hello"}]` + tc.fields + `}`)
			var mappings []ReasoningEffortMapping
			if tc.from != "" {
				mappings = []ReasoningEffortMapping{{From: tc.from, To: tc.to, MatchType: domain.ReasoningEffortMatchExact, Model: tc.model}}
			}
			ctx := WithOpenAIReasoningEffortPolicyForMessages(context.Background(), body, tc.max, mappings, tc.action)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body)).WithContext(ctx)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_effort\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"))}}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			account := rawChatCompletionsTestAccount()
			account.Extra = map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses)}
			if tc.mapped != "" {
				account.Credentials["model_mapping"] = map[string]any{tc.model: tc.mapped}
			}
			result, err := svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
			_, countErr := prepareOpenAIInputTokensCountRequest(body, account, "", ctx)
			if tc.status != 0 {
				require.Error(t, err)
				require.Error(t, countErr)
				require.Equal(t, tc.status, recorder.Code)
				require.Nil(t, upstream.lastReq)
				return
			}
			require.NoError(t, err)
			require.NoError(t, countErr)
			require.NotNil(t, result.ReasoningEffort)
			require.Equal(t, tc.want, gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
			require.Equal(t, tc.want, *result.ReasoningEffort)
			if tc.from == "none" {
				require.False(t, gjson.GetBytes(upstream.lastBody, "temperature").Exists())
				require.False(t, gjson.GetBytes(upstream.lastBody, "top_p").Exists())
			}
			wantModel := tc.model
			if tc.mapped != "" {
				wantModel = tc.mapped
			}
			require.Equal(t, wantModel, gjson.GetBytes(upstream.lastBody, "model").String())
		})
	}
}
