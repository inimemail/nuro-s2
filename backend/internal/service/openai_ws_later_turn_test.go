package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWSLaterTurnFailureCannotBeUsedAsFirstFrameFailover(t *testing.T) {
	for _, tc := range []struct {
		name      string
		event     string
		forwarded bool
	}{
		{name: "rate_limit", event: `{"type":"error","error":{"code":"rate_limit_exceeded","message":"fixture"}}`},
		{name: "unknown_execution_eof"},
		{name: "terminal_error", event: `{"type":"error","error":{"code":"server_error","message":"fixture"}}`, forwarded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS = config.GatewayOpenAIWSConfig{Enabled: true, APIKeyEnabled: true, ResponsesWebsocketsV2: true, MaxConnsPerAccount: 1, MaxIdlePerAccount: 1, QueueLimitPerConn: 8, DialTimeoutSeconds: 3, ReadTimeoutSeconds: 3, WriteTimeoutSeconds: 3}
			upstream := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)}}
			if tc.event != "" {
				upstream.events = append(upstream.events, []byte(tc.event))
			}
			dialer := &openAIWSCaptureDialer{conn: upstream}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
			account := &Account{ID: 9991, Name: "fixture", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "sk-fixture"}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
			result := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					result <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_, first, err := conn.Read(r.Context())
				if err != nil {
					result <- err
					return
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r
				result <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, account, "sk-fixture", first, nil)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"first"}`)))
			_, first, err := client.Read(ctx)
			require.NoError(t, err)
			require.Contains(t, string(first), "resp_1")
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"second","previous_response_id":"resp_1"}`)))
			if tc.forwarded {
				// A forwarded protocol error completes the turn and waits for the
				// client. Close it explicitly rather than expecting failover.
				_, terminal, err := client.Read(ctx)
				require.NoError(t, err)
				require.Contains(t, string(terminal), `"response.failed"`)
				require.NoError(t, client.CloseNow())
			}
			select {
			case err := <-result:
				if tc.forwarded {
					require.NoError(t, err)
				} else {
					var later *OpenAIWSLaterTurnError
					require.ErrorAs(t, err, &later)
				}
			case <-ctx.Done():
				t.Fatal("later turn did not finish")
			}
			require.Equal(t, 1, dialer.DialCount())
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			require.Len(t, upstream.writes, 2, "the first frame must never be sent again")
		})
	}
}
