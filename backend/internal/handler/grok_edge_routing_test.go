package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type grokEdgeTestTransport func(*http.Request) (*http.Response, error)

func (f grokEdgeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenAIEdgeIngressPlatformRouting(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		for _, tc := range []struct {
			platform, resolved string
			wantEdge           bool
		}{
			{service.PlatformOpenAI, "", true},
			{service.PlatformGrok, "", false},
			{service.PlatformKimi, "", false},
			{service.PlatformDeepSeek, "", false},
			{service.PlatformZhipu, "", false},
			{service.PlatformMiniMax, "", false},
			{service.PlatformOpenCodeGo, "", false},
			{service.PlatformComposite, "", false},
			{service.PlatformComposite, service.PlatformOpenAI, false},
			{service.PlatformComposite, service.PlatformGrok, false},
			{service.PlatformOpenAI, service.PlatformGrok, false},
		} {
			t.Run(path+"/"+tc.platform+"/"+tc.resolved, func(t *testing.T) {
				original := openAIEdgeIngressClient
				t.Cleanup(func() { openAIEdgeIngressClient = original })
				calls := 0
				body := `{"model":"grok-4.6","input":"hi","stream":true}`
				openAIEdgeIngressClient = &http.Client{Transport: grokEdgeTestTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPut {
						return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: http.NoBody}, nil
					}
					calls++
					got, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Equal(t, body, string(got))
					require.Equal(t, path, r.URL.Path)
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))}, nil
				})}
				h := &OpenAIGatewayHandler{cfg: &config.Config{}}
				h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true,
					InternalSecret: "test-secret", Mode: "relay", IngressProxyEnabled: true, ListenAddr: "127.0.0.1:18080"}
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: tc.platform}})
				if tc.resolved != "" {
					c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), tc.resolved))
				}
				require.Equal(t, tc.wantEdge, h.tryOpenAIEdgeIngressProxy(c))
				if tc.wantEdge {
					require.Equal(t, 1, calls)
					require.Contains(t, w.Body.String(), "response.completed")
				} else {
					require.Zero(t, calls)
					require.False(t, c.Writer.Written())
					got, err := io.ReadAll(c.Request.Body)
					require.NoError(t, err)
					require.Equal(t, body, string(got), "Go must receive the original request")
				}
			})
		}
	}
}

type edgePlatformAPIKeyRepo struct {
	service.APIKeyRepository
	key *service.APIKey
}

func (r *edgePlatformAPIKeyRepo) GetByKeyForAuth(context.Context, string) (*service.APIKey, error) {
	return r.key, nil
}

func (r *edgePlatformAPIKeyRepo) UpdateLastUsed(context.Context, int64, time.Time) error { return nil }

func TestOpenAIEdgePrepareNonOpenAIPlatformFallsBackBeforeScheduling(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
		for _, platform := range []string{service.PlatformGrok, service.PlatformKimi, service.PlatformDeepSeek,
			service.PlatformZhipu, service.PlatformMiniMax, service.PlatformOpenCodeGo, service.PlatformComposite} {
			t.Run(path+"/"+platform, func(t *testing.T) {
				groupID := int64(91)
				key := &service.APIKey{ID: 91, UserID: 91, GroupID: &groupID, Status: service.StatusActive,
					User:  &service.User{ID: 91, Status: service.StatusActive, Concurrency: 1},
					Group: &service.Group{ID: groupID, Platform: platform, Status: service.StatusActive, Hydrated: true}}
				h := &OpenAIGatewayHandler{cfg: &config.Config{},
					apiKeyService:  service.NewAPIKeyService(&edgePlatformAPIKeyRepo{key: key}, nil, nil, nil, nil, nil, nil),
					gatewayService: &service.OpenAIGatewayService{}, billingCacheService: &service.BillingCacheService{},
					concurrencyHelper: &ConcurrencyHelper{}}
				h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true,
					InternalSecret: "test-secret", Mode: "relay", RelayResponses: true, RelayChatCompletions: true, RolloutPercent: 100}
				body := `{"edge_request_id":"edge-grok","method":"POST","path":"` + path + `","headers":{"Authorization":"Bearer test-key"},"body":{"model":"grok-4.6","input":"hi","stream":true},"stream":true}`
				c, w := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/prepare", body, "test-secret")
				h.OpenAIEdgePrepare(c)
				require.Equal(t, http.StatusOK, w.Code)
				var plan service.OpenAIEdgePlan
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &plan))
				require.Equal(t, service.OpenAIEdgeActionFallbackGo, plan.Action)
				require.Equal(t, "platform_requires_go", plan.Reason)
				require.Empty(t, plan.LeaseID)
			})
		}
	}
}

func TestOpenAIEdgeIngressErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantType, wantMessage string
		status                            int
	}{
		{"capacity", `{"error":{"type":"api_error","message":"Service temporarily unavailable","secret":"private"}}`, "api_error", "Service temporarily unavailable", 503},
		{"compact", `{"error":{"type":"compact_not_supported","message":"No available accounts support native remote compaction v2"}}`, "compact_not_supported", "No available accounts support native remote compaction v2", 503},
		{"html", "<html>private-provider.example secret</html>", "upstream_error", "Upstream request failed", 503},
		{"untrusted_json", `{"error":{"type":"api_error","message":"Authorization=Bearer private-token"}}`, "upstream_error", "Upstream request failed", 503},
		{"wrong_status", `{"error":{"type":"api_error","message":"Service temporarily unavailable"}}`, "upstream_error", "Upstream request failed", 403},
		{"oversized", strings.Repeat(" ", 8193) + `{"error":{"type":"api_error","message":"Service temporarily unavailable"}}`, "upstream_error", "Upstream request failed", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, message := openAIEdgeIngressClientError(tc.status, strings.NewReader(tc.body))
			require.Equal(t, tc.wantType, kind)
			require.Equal(t, tc.wantMessage, message)
		})
	}
}

type edgeErrorContextBody struct{ ctx context.Context }

func (b edgeErrorContextBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b edgeErrorContextBody) Close() error { return nil }

func TestOpenAIEdgeIngressErrorResponseSanitizedAndBounded(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(map[bool]string{false: "public_error", true: "incomplete_error_body"}[stalled], func(t *testing.T) {
			original := openAIEdgeIngressClient
			t.Cleanup(func() { openAIEdgeIngressClient = original })
			openAIEdgeIngressClient = &http.Client{Transport: grokEdgeTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPut {
					return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: http.NoBody}, nil
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader(`{"error":{"type":"api_error","message":"Service temporarily unavailable","secret":"private-token"}}`))
				if stalled {
					body = edgeErrorContextBody{ctx: r.Context()}
				}
				return &http.Response{StatusCode: http.StatusServiceUnavailable,
					Header: http.Header{"Server": {"private-provider"}, "X-Request-Id": {"private-id"}}, Body: body}, nil
			})}
			h := &OpenAIGatewayHandler{cfg: &config.Config{}}
			h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true,
				InternalSecret: "test-secret", Mode: "relay", IngressProxyEnabled: true, ListenAddr: "127.0.0.1:18080"}
			c, w := newOpenAIEdgeTestContext(http.MethodPost, "/v1/responses", `{"model":"gpt-test","input":"hi","stream":true}`, "")
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
			require.True(t, h.tryOpenAIEdgeIngressProxy(c))
			require.NoError(t, c.Request.Context().Err(), "classifying an Edge error must not cancel the caller context")
			require.Equal(t, http.StatusServiceUnavailable, w.Code)
			if stalled {
				require.JSONEq(t, `{"error":{"type":"upstream_error","message":"Upstream request failed"}}`, w.Body.String())
			} else {
				require.JSONEq(t, `{"error":{"type":"api_error","message":"Service temporarily unavailable"}}`, w.Body.String())
			}
			require.Empty(t, w.Header().Get("Server"))
			require.Empty(t, w.Header().Get("X-Request-Id"))
			require.NotContains(t, w.Body.String(), "private")
		})
	}
}

func TestOpenAIEdgeIngressRealTransportTimeoutScope(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, stalledError := range []bool{false, true} {
			t.Run(fmt.Sprintf("http2=%t/stalled_error=%t", http2, stalledError), func(t *testing.T) {
				// A real HTTP body is needed here: a fake context-aware Reader does
				// not verify that Transport cancels HTTP/1 and HTTP/2 body reads.
				protocol := make(chan int, 1)
				finished := make(chan struct{})
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPut {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					defer close(finished)
					protocol <- r.ProtoMajor
					if stalledError {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusServiceUnavailable)
						w.(http.Flusher).Flush()
						<-r.Context().Done()
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-time.After(1200 * time.Millisecond):
						_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
					case <-r.Context().Done():
					}
				}))
				server.EnableHTTP2 = http2
				server.StartTLS()
				t.Cleanup(server.Close)
				original := openAIEdgeIngressClient
				openAIEdgeIngressClient = server.Client()
				t.Cleanup(func() { openAIEdgeIngressClient = original })
				h := &OpenAIGatewayHandler{cfg: &config.Config{}}
				h.cfg.Gateway.OpenAIEdgeRS = config.GatewayOpenAIEdgeRSConfig{Enabled: true, InternalAPIEnabled: true,
					InternalSecret: "test-secret", Mode: "relay", IngressProxyEnabled: true, ListenAddr: server.URL}
				c, w := newOpenAIEdgeTestContext(http.MethodPost, "/v1/responses", `{"model":"gpt-test","input":"hi","stream":true}`, "")
				ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
				require.True(t, h.tryOpenAIEdgeIngressProxy(c))
				require.NoError(t, ctx.Err(), "the bounded Edge read must finish without canceling or exhausting the parent context")
				wantProtocol := 1
				if http2 {
					wantProtocol = 2
				}
				require.Equal(t, wantProtocol, <-protocol)
				if stalledError {
					require.Equal(t, http.StatusServiceUnavailable, w.Code)
					require.JSONEq(t, `{"error":{"type":"upstream_error","message":"Upstream request failed"}}`, w.Body.String())
				} else {
					require.Equal(t, http.StatusOK, w.Code)
					require.Contains(t, w.Body.String(), "response.created")
					require.Contains(t, w.Body.String(), "response.completed", "successful streams must outlive the one-second error-body budget")
					require.NotContains(t, w.Body.String(), "response.failed")
				}
				select {
				case <-finished:
				case <-ctx.Done():
					t.Fatal("Edge transport did not release its HTTP request")
				}
			})
		}
	}
}
