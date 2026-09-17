package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAICompactionOutputSurvivesPlaceholderCoordination(t *testing.T) {
	setGinTestMode()
	item := `{"id":"cmp_test","type":"compaction","encrypted_content":"opaque-state"}`
	for _, passthrough := range []bool{false, true} {
		for _, itemEvent := range []string{"response.output_item.added", "response.output_item.done", "terminal_only"} {
			for _, placeholderSent := range []bool{false, true} {
				t.Run(fmt.Sprintf("passthrough=%t/%s/placeholder_sent=%t", passthrough, itemEvent, placeholderSent), func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					started := time.Now()
					StartOpenAIPlaceholderCoordination(c, started)
					openAIPlaceholderCoordinatorFromContext(c).activate()
					if placeholderSent {
						require.True(t, writeOpenAIRequestFirstTokenTimeoutPlaceholder(c, started, "gpt-5.6-sol", openAIRequestFirstTokenPlaceholderDialectResponses).Sent)
					}
					sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_compact\"}}\n\n"
					if itemEvent != "terminal_only" {
						sse += fmt.Sprintf("data: {\"type\":%q,\"output_index\":0,\"item\":%s}\n\n", itemEvent, item)
					}
					sse += fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact\",\"status\":\"completed\",\"output\":[%s],\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n", item)
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}
					account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
					svc := &OpenAIGatewayService{}
					var err error
					if passthrough {
						_, err = svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, started, "gpt-5.6-sol", "gpt-5.6-sol")
					} else {
						_, err = svc.handleStreamingResponse(c.Request.Context(), resp, c, account, started, "gpt-5.6-sol", "gpt-5.6-sol")
					}
					require.NoError(t, err)
					body := rec.Body.String()
					require.Contains(t, body, `"encrypted_content":"opaque-state"`)
					require.Equal(t, 1, strings.Count(body, `"type":"response.completed"`))
					require.NotContains(t, body, `"type":"response.failed"`)
					require.True(t, OpenAIRequestUpstreamCommitted(c))
					require.True(t, OpenAIRequestResponsesTerminalWritten(c))
					if placeholderSent {
						require.Equal(t, 1, strings.Count(body, `"type":"response.transport_progress.delta"`))
					}
				})
			}
		}
	}
}
