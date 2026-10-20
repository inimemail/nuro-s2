package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/alicebob/miniredis/v2"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIPoolDisablePreservesTransportProtection(t *testing.T) {
	for _, class := range []string{"upstream_no_progress", "upstream_disconnect"} {
		t.Run(class, func(t *testing.T) {
			s := &OpenAIGatewayService{}
			a := &Account{ID: 901, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_soft_cooldown_enabled": false}}
			s.RecordOpenAIEdgeStreamOutcome(a, class, false, false, "confirmed upstream fault")
			if class == "upstream_disconnect" {
				s.RecordOpenAIEdgeStreamOutcome(a, class, false, false, "confirmed upstream fault")
			}
			before := s.isOpenAIAccountRuntimeBlocked(a)
			s.RecordOpenAIPoolFailureAfterCommittedResponse(context.Background(), a, 502, []byte("upstream failure"), "gpt-test", "upstream failure")
			after := s.isOpenAIAccountRuntimeBlocked(a)
			t.Logf("confirmed runtime protection cleared: before=%v after=%v", before, after)
			if !before || !after {
				t.Fatal("reproduction did not match")
			}
			if class == "upstream_disconnect" {
				s.RecordOpenAIEdgeStreamOutcome(a, class, false, false, "third disconnect")
				third := s.isOpenAIAccountRuntimeBlocked(a)
				t.Logf("third disconnect restores protection=%v", third)
				if !third {
					t.Fatal("third-disconnect reproduction did not match")
				}
			}
		})
	}
}
func TestOpenAICleanEOFCountedOnBothStreamPaths(t *testing.T) {
	for _, passthrough := range []bool{true, false} {
		s := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
		a := &Account{ID: 902, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
		var err error
		if passthrough {
			_, err = s.handleStreamingResponsePassthrough(context.Background(), resp, c, a, time.Now(), "gpt-test", "gpt-test")
		} else {
			_, err = s.handleStreamingResponse(context.Background(), resp, c, a, time.Now(), "gpt-test", "gpt-test")
		}
		_, counted := s.openaiAccountStreamDisconnectCount.Load(a.ID)
		t.Logf("passthrough=%v disconnect_counted=%v error=%v", passthrough, counted, err)
		if err == nil || !counted {
			t.Fatal("reproduction did not match")
		}
	}
}

func TestOpenAIRepeatedFailureDeadlineDoesNotSlide(t *testing.T) {
	s := &OpenAIGatewayService{}
	a := &Account{ID: 991, Platform: PlatformOpenAI}
	s.RecordOpenAIEdgeStreamOutcome(a, "upstream_error", false, false, "server error")
	if s.isOpenAIAccountRuntimeBlocked(a) {
		t.Fatal("first service error blocked account")
	}
	s.RecordOpenAIEdgeStreamOutcome(a, "upstream_error", false, false, "server error")
	first, _ := s.openaiAccountRuntimeBlockUntil.Load(a.ID)
	until, _, ok := parseAccountRuntimeDeadline(first)
	if !ok {
		t.Fatal("missing block")
	}
	s.clearLocalAccountRuntimeBlock(a.ID) // Repair a missed runtime write at count > threshold.
	s.RecordOpenAIEdgeStreamOutcome(a, "upstream_disconnect", false, false, "disconnect")
	next, _ := s.openaiAccountRuntimeBlockUntil.Load(a.ID)
	repaired, _, ok := parseAccountRuntimeDeadline(next)
	if !ok || !until.Equal(repaired) {
		t.Fatalf("deadline moved: %v -> %v", until, repaired)
	}
}

func TestOpenAIWSProgressGuardEffectiveScopeAndCancellation(t *testing.T) {
	s := &OpenAIGatewayService{cfg: &config.Config{}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	disabled := false
	c.Set("api_key", &APIKey{Group: &Group{EdgeProtectionEnabled: &disabled}})
	if s.newOpenAIWSProgressGuard(c).timeout != 0 {
		t.Fatal("group disable was ignored")
	}
	guard := &openAIWSProgressGuard{timeout: time.Second, remaining: time.Second}
	guard.consumedRead(750 * time.Millisecond)
	guard.observe([]byte(`{"type":"response.in_progress"}`), "response.in_progress")
	if guard.remaining != 250*time.Millisecond {
		t.Fatal("empty progress refreshed guard")
	}
	guard.observe([]byte(`{"type":"response.reasoning_text.delta","delta":"thinking"}`), "response.reasoning_text.delta")
	if guard.remaining != time.Second {
		t.Fatal("reasoning did not refresh guard")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if openAIWSReadError(ctx, context.DeadlineExceeded, true) != context.DeadlineExceeded {
		t.Fatal("client cancellation became upstream failure")
	}
	err := openAIWSReadError(context.Background(), context.DeadlineExceeded, true)
	if _, ok := err.(*openAIWSUpstreamTimeoutError); !ok {
		t.Fatal("upstream deadline lost")
	}
}

func TestOpenAIWSNoProgressReachesProtectionOnHTTPAndIngress(t *testing.T) {
	for _, mode := range []string{"http", OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = mode == OpenAIWSIngressModePassthrough
			cfg.SetGatewaySchedulingRuntime(config.GatewaySchedulingRuntime{
				EdgeProtectionConfigured: true, EdgeProtectionEnabled: true, EdgeBodyIdleTimeoutMS: 100,
			})
			pool := newOpenAIWSConnPool(cfg)
			t.Cleanup(pool.Close)
			upstream := &openAIWSCaptureConn{}
			for i := 0; i < 20; i++ {
				upstream.readDelays = append(upstream.readDelays, 15*time.Millisecond)
				upstream.events = append(upstream.events, []byte(`{"type":"response.in_progress"}`))
			}
			dialer := &openAIWSCaptureDialer{conn: upstream}
			pool.setClientDialerForTest(dialer)
			s := &OpenAIGatewayService{cfg: cfg, cache: &stubGatewayCache{}, httpUpstream: &httpUpstreamRecorder{},
				openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSPool: pool, toolCorrector: NewCodexToolCorrector(),
				openaiWSPassthroughDialer: dialer}
			account := &Account{ID: 994, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
			if mode == OpenAIWSIngressModePassthrough {
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			}
			if mode == "http" {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				_, err := s.Forward(context.Background(), c, account, []byte(`{"model":"gpt-test","input":"hi","stream":false}`))
				require.Error(t, err)
				require.True(t, s.isOpenAIAccountRuntimeBlocked(account), "HTTP-to-WS timeout must reach transport protection")
				return
			}
			hookError := make(chan error, 1)
			serverError := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					serverError <- err
					return
				}
				defer conn.CloseNow()
				_, body, err := conn.Read(r.Context())
				if err != nil {
					serverError <- err
					return
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r
				serverError <- s.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, account, "sk-test", body, &OpenAIWSIngressHooks{
					AfterTurn: func(_ int, _ *OpenAIForwardResult, turnErr error) { hookError <- turnErr },
				})
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer conn.CloseNow()
			require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-test","input":"hi"}`)))
			select {
			case err := <-hookError:
				require.True(t, OpenAIWSIngressRequestWasWritten(err), "AfterTurn lost request-written evidence")
				require.True(t, IsOpenAIWSUpstreamDisconnectError(err), "AfterTurn lost the upstream stage")
				s.RecordOpenAIAccountWSFailure(account, "gpt-test", err)
				require.True(t, s.isOpenAIAccountRuntimeBlocked(account))
			case <-ctx.Done():
				t.Fatal("missing failed turn callback")
			}
			select {
			case err := <-serverError:
				require.True(t, OpenAIWSIngressRequestWasWritten(err), "returned error lost replay evidence")
			case <-ctx.Done():
				t.Fatal("WS relay did not terminate")
			}
		})
	}
}

func TestOpenAIStreamFailureAvoidanceExcludesCompatibilityAndModelRefusals(t *testing.T) {
	for _, errorBody := range []string{
		`{"code":"previous_response_not_found"}`,
		`{"code":"invalid_encrypted_content"}`,
		`{"code":"model_not_found"}`,
		`{"code":"model_not_supported"}`,
		`{"message":"this account does not support this model"}`,
		`{"message":"Upstream account cannot serve requested model; check model mapping or upstream group config"}`,
		`{"message":"unsupported parameter: prompt_cache_retention"}`,
		`{"message":"unsupported parameter: prompt_cache_key"}`,
		`{"status":502,"message":"unsupported parameter: reasoning"}`,
	} {
		payload := []byte(`{"type":"response.failed","response":{"error":` + errorBody + `}}`)
		require.False(t, OpenAIStreamFailureCountsForAccount(payload, ""), "must preserve compatibility/model handling: %s", errorBody)
	}
	require.True(t, OpenAIStreamFailureCountsForAccount([]byte(`{"type":"response.failed","response":{"error":{"code":"server_error"}}}`), ""))
}

func TestOpenAIStreamHealthReceiptDeduplicatesAcrossReplicas(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	a := &Account{ID: 992, Platform: PlatformOpenAI}
	first := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	second := &OpenAIGatewayService{openaiAccountHealthRedis: client}
	for _, s := range []*OpenAIGatewayService{first, second} {
		if err := s.RecordOpenAIStreamHealthEvent(context.Background(), "edge:lease:turn1", a, "gpt-test", "upstream_error", false, nil); err != nil {
			t.Fatal(err)
		}
	}
	count, err := client.Get(context.Background(), "openai:account_stream_disconnect:992").Int()
	if err != nil || count != 1 {
		t.Fatalf("duplicate health callback counted: %v %v", count, err)
	}
	if err := second.RecordOpenAIStreamHealthEvent(context.Background(), "edge:lease:turn2", a, "gpt-test", "upstream_error", false, nil); err != nil {
		t.Fatal(err)
	}
	if !second.isOpenAIAccountRuntimeBlocked(a) {
		t.Fatal("second failed turn did not protect account")
	}
}

func TestOpenAIStreamHealthLocalReceiptPrunesExpiredOldestEntry(t *testing.T) {
	now := time.Now()
	s := &OpenAIGatewayService{
		openaiStreamHealthReceipts: make(map[string]time.Time, 8193),
	}
	for i := 0; i < 8193; i++ {
		expiry := now.Add(time.Hour + time.Duration(i))
		if i == 0 {
			expiry = now.Add(-time.Second)
		}
		s.openaiStreamHealthReceipts[fmt.Sprintf("receipt-%d", i)] = expiry
	}
	account := &Account{ID: 993, Platform: PlatformOpenAI}
	if err := s.RecordOpenAIStreamHealthEvent(context.Background(), "edge:lease:bounded", account, "gpt-test", "", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(s.openaiStreamHealthReceipts); got != 8192 {
		t.Fatalf("local receipt cache exceeded its bound: got %d entries", got)
	}
}
