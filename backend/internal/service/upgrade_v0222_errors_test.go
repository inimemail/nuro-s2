package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0222CommittedErrorsStayInProtocolAndEmitOnce(t *testing.T) {
	for _, dialect := range []string{"chat", "responses", "anthropic"} {
		t.Run(dialect, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest("POST", "/", nil)
			c.Header("Content-Type", "text/event-stream")
			c.Writer.WriteHeaderNow()
			require.True(t, WriteGatewayCommittedError(c, dialect, "upstream_error", "safe failure"))
			first := recorder.Body.String()
			require.True(t, WriteGatewayCommittedError(c, dialect, "upstream_error", "duplicate"))
			require.Equal(t, first, recorder.Body.String())
			switch dialect {
			case "responses":
				data := strings.Split(strings.Split(first, "data: ")[1], "\n")[0]
				require.Equal(t, "response.failed", gjson.Get(data, "type").String())
				require.Equal(t, int64(0), gjson.Get(data, "sequence_number").Int())
				require.True(t, strings.HasPrefix(gjson.Get(data, "response.id").String(), "resp_"))
				require.Equal(t, "failed", gjson.Get(data, "response.status").String())
				require.Equal(t, "upstream_error", gjson.Get(data, "response.error.code").String())
			case "chat":
				require.Equal(t, 1, strings.Count(first, "[DONE]"))
			default:
				require.Contains(t, first, "event: error")
			}
		})
	}
}
func TestV0222CommittedErrorCancellationAndJSON(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	c.Header("Content-Type", "text/event-stream")
	c.Writer.WriteHeaderNow()
	require.True(t, WriteGatewayCommittedError(c, "chat", "error", "cancelled"))
	require.Empty(t, recorder.Body.String())
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.False(t, WriteGatewayCommittedError(c2, "chat", "error", "json"))
}
func TestV0222FableMissingUtilizationCannotRecover(t *testing.T) {
	svc := &AccountUsageService{}
	for _, body := range []string{`{"seven_day_overage_included":{"resets_at":"2026-12-01T00:00:00Z"}}`, `{"seven_day_overage_included":{"utilization":null,"resets_at":"2026-12-01T00:00:00Z"}}`} {
		var response ClaudeUsageResponse
		require.NoError(t, json.Unmarshal([]byte(body), &response))
		require.Nil(t, svc.buildUsageInfo(&response, nil).SevenDayFable)
	}
	for _, body := range []string{`{"seven_day_overage_included":{"utilization":0,"resets_at":"2026-12-01T00:00:00Z"}}`, `{"limits":[{"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2026-12-01T00:00:00Z","scope":{"model":{"display_name":"Fable"},"surface":null}}]}`} {
		var response ClaudeUsageResponse
		require.NoError(t, json.Unmarshal([]byte(body), &response))
		progress := svc.buildUsageInfo(&response, nil).SevenDayFable
		require.NotNil(t, progress)
		require.Zero(t, progress.Utilization)
	}
}

type v0222FableRecoveryRepo struct {
	AccountRepository
	cleared [][]byte
}

func (r *v0222FableRecoveryRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}

func (r *v0222FableRecoveryRepo) ClearModelRateLimitIfOwned(_ context.Context, _ int64, _ string, expected []byte) error {
	r.cleared = append(r.cleared, append([]byte(nil), expected...))
	return nil
}

type v0222FableUsageFetcher struct {
	ClaudeUsageFetcher
	response *ClaudeUsageResponse
	calls    int
}

func (f *v0222FableUsageFetcher) FetchUsageWithOptions(context.Context, *ClaudeUsageFetchOptions) (*ClaudeUsageResponse, error) {
	f.calls++
	return f.response, nil
}

func TestV0222FableRecoveryRequiresFreshCurrentWindow(t *testing.T) {
	for _, tc := range []struct {
		name, reset string
		cached      bool
		wantClear   bool
	}{
		{"cached quota predates new restriction", time.Now().Add(time.Hour).Format(time.RFC3339), true, false},
		{"fresh current quota", time.Now().Add(time.Hour).Format(time.RFC3339), false, true},
		{"fresh malformed window", "not-a-timestamp", false, false},
		{"fresh expired window", time.Now().Add(-time.Hour).Format(time.RFC3339), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := 0.0
			response := &ClaudeUsageResponse{SevenDayOverageIncluded: ClaudeUsageWindow{Utilization: &value, ResetsAt: tc.reset}}
			fetcher := &v0222FableUsageFetcher{response: response}
			repo := &v0222FableRecoveryRepo{}
			account := &Account{ID: 51, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"access_token": "fixture"}, Extra: map[string]any{
					modelRateLimitsKey: map[string]any{anthropicFableRateLimitKey: map[string]any{
						"reason": anthropicFableWindowReason, "owner_token": "new-restriction",
					}},
				}}
			cache := NewUsageCache()
			cache.windowStatsCache.Store(account.ID, &windowStatsCache{stats: &WindowStats{}, timestamp: time.Now()})
			if tc.cached {
				cache.apiCache.Store(account.ID, &apiUsageCache{response: response, timestamp: time.Now().Add(-time.Minute)})
			}
			svc := &AccountUsageService{accountRepo: repo, usageFetcher: fetcher, cache: cache}
			usage, err := svc.GetUsageForAccount(context.Background(), account)
			require.NoError(t, err)
			require.NotNil(t, usage.SevenDayFable, "keep the existing display even when it cannot authorize recovery")
			if tc.cached {
				require.Zero(t, fetcher.calls, "do not bypass the usage cache or add another upstream query")
			} else {
				require.Equal(t, 1, fetcher.calls)
			}
			if tc.wantClear {
				require.Len(t, repo.cleared, 1)
				require.Equal(t, "new-restriction", gjson.GetBytes(repo.cleared[0], "owner_token").String())
				// A new restriction may arrive after this observation was cached.
				// Reading the cached success again must not clear it a second time.
				_, err = svc.GetUsageForAccount(context.Background(), account)
				require.NoError(t, err)
				require.Len(t, repo.cleared, 1)
				require.Equal(t, 1, fetcher.calls)
			} else {
				require.Empty(t, repo.cleared)
			}
		})
	}
}

func TestV0222AntigravityThoughtOnlyDoesNotAdvertiseSuccess(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/", nil)
			c.Header("Content-Type", "text/event-stream")
			writer := newAntigravityClientWriter(c.Writer, c.Writer, "fixture")
			var adapter antigravityCompatStreamAdapter = newAntigravityChatStreamAdapter("public", false)
			if protocol == "responses" {
				adapter = newAntigravityResponsesStreamAdapter("public")
			}
			session := newAntigravityCompatStreamSession("public", time.Now(), adapter, writer)
			session.consumeClaudeData("message_start", `{"type":"message_start","message":{"id":"msg_fixture","model":"provider","role":"assistant","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}`)
			session.consumeClaudeData("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought"}}`)
			session.consumeClaudeData("message_stop", `{"type":"message_stop"}`)
			require.False(t, session.hasMeaningfulData())
			require.Nil(t, session.firstTokenMs)
			require.NotContains(t, rec.Body.String(), "response.completed")
			require.NotContains(t, rec.Body.String(), "[DONE]")
			writeAntigravityCompatStreamError(c, adapter, writer, "empty_stream")
			first := rec.Body.String()
			writeAntigravityCompatStreamError(c, adapter, writer, "empty_stream")
			require.Equal(t, first, rec.Body.String())
			if protocol == "responses" {
				require.Contains(t, first, "response.failed")
			} else {
				require.Equal(t, 1, strings.Count(first, "[DONE]"))
			}
			require.Equal(t, 5, session.usage.InputTokens)
		})
	}
}
func TestV0222AntigravityReadErrorBeforeVisibleContent(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, commitment := range []string{"uncommitted", "queue_ping", "thinking"} {
			for _, readErr := range []error{io.ErrUnexpectedEOF, bufio.ErrTooLong} {
				t.Run(protocol+"/"+commitment+"/"+readErr.Error(), func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest("POST", "/", nil)
					c.Header("Content-Type", "text/event-stream")
					writer := newAntigravityClientWriter(c.Writer, c.Writer, "fixture")
					var adapter antigravityCompatStreamAdapter = newAntigravityChatStreamAdapter("public", false)
					if protocol == "responses" {
						adapter = newAntigravityResponsesStreamAdapter("public")
					}
					session := newAntigravityCompatStreamSession("public", time.Now(), adapter, writer)
					session.consumeClaudeData("message_start", `{"type":"message_start","message":{"id":"msg_fixture","model":"provider","role":"assistant","content":[],"usage":{"input_tokens":5,"output_tokens":3}}}`)
					switch commitment {
					case "queue_ping":
						_, err := c.Writer.Write([]byte(": ping\n\n"))
						require.NoError(t, err)
						c.Writer.Flush()
					case "thinking":
						session.consumeClaudeData("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought"}}`)
					}
					require.False(t, session.hasMeaningfulData())
					result, err := (&AntigravityGatewayService{}).handleAntigravityCompatReadError(c, session, readErr, defaultMaxLineSize, "fixture")
					var failoverErr *UpstreamFailoverError
					if commitment == "uncommitted" {
						require.ErrorAs(t, err, &failoverErr)
						require.Nil(t, result)
						require.Empty(t, rec.Body.String())
						return
					}
					require.ErrorIs(t, err, errAntigravityCommittedNoContent)
					require.False(t, errors.As(err, &failoverErr), "committed errors cannot authorize failover")
					require.NotNil(t, result, "preserve observed usage for settlement")
					require.Equal(t, 5, result.usage.InputTokens)
					require.Equal(t, 3, result.usage.OutputTokens)
					require.Nil(t, result.firstTokenMs)
					require.False(t, result.clientDisconnect)
					first := rec.Body.String()
					writeAntigravityCompatStreamError(c, adapter, writer, "duplicate")
					require.Equal(t, first, rec.Body.String())
					if protocol == "responses" {
						require.Equal(t, 1, strings.Count(first, "event: response.failed"))
						require.NotContains(t, first, "response.completed")
					} else {
						require.Equal(t, 1, strings.Count(first, "[DONE]"))
					}
				})
			}
		}
	}
}

func TestV0222CompositeRestoresOnlyResponseEnvelope(t *testing.T) {
	ctx := context.WithValue(context.Background(), compositeContextKey("public_response_model"), "public")
	original := []byte(`{"type":"message_start","message":{"model":"provider","content":[{"type":"tool_use","input":{"model":"tool-model"}}]}}`)
	restored := restoreCompositeAnthropicResponseModel(ctx, original, true)
	require.Equal(t, "public", gjson.GetBytes(restored, "message.model").String())
	require.Equal(t, "tool-model", gjson.GetBytes(restored, "message.content.0.input.model").String())
	delta := []byte(`{"type":"content_block_delta","delta":{"model":"provider"}}`)
	require.Equal(t, delta, restoreCompositeAnthropicResponseModel(ctx, delta, true))
	require.Equal(t, original, restoreCompositeAnthropicResponseModel(context.Background(), original, true))
}
func TestV0222AnthropicTierIsAuditOnly(t *testing.T) {
	o := &upstreamResponseModelObserver{}
	o.ObserveAnthropic([]byte(`{"type":"message_start","message":{"model":"provider","usage":{"service_tier":"standard"}}}`))
	require.Equal(t, "standard", o.anthropicServiceTier)
	require.Empty(t, o.serviceTier)
	o.ObserveAnthropic([]byte(`{"usage":{"service_tier":"secret arbitrary payload"}}`))
	require.Equal(t, "standard", o.anthropicServiceTier)
}
