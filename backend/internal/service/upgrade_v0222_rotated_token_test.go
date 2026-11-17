//go:build unit

package service

import (
	"bytes"
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type v0222RotatedRepo struct {
	AccountRepository
	latest *Account
	calls  int
}

func (r *v0222RotatedRepo) GetByID(ctx context.Context, _ int64) (*Account, error) {
	r.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.latest, nil
}

type v0222AuthUpstream struct {
	geminiSequenceUpstreamStub
	tokens []string
}

func (s *v0222AuthUpstream) DoWithTLS(req *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	s.tokens = append(s.tokens, getHeaderRaw(req.Header, "Authorization"))
	return s.Do(req, p, id, n)
}
func TestV0222Claude401RetriesOnlyAnAlreadyRotatedTokenOnce(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		for _, rotated := range []bool{false, true} {
			t.Run(protocol+map[bool]string{true: "-rotated", false: "-same"}[rotated], func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.Enabled = false
				account := &Account{ID: 91, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "old", "base_url": "https://fixture.invalid"}}
				latest := *account
				latest.Credentials = map[string]any{"access_token": "old"}
				if rotated {
					latest.Credentials["access_token"] = "new"
				}
				repo := &v0222RotatedRepo{latest: &latest}
				up := &v0222AuthUpstream{geminiSequenceUpstreamStub: geminiSequenceUpstreamStub{responses: []geminiSequenceUpstreamResponse{{status: 401, body: `{"error":{"type":"authentication_error","message":"rejected"}}`}}}}
				svc := &GatewayService{cfg: cfg, accountRepo: repo, httpUpstream: up}
				body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`)
				if protocol == "responses" {
					body = []byte(`{"model":"claude-sonnet-4-6","input":"hello"}`)
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/"+protocol, bytes.NewReader(body))
				var err error
				if protocol == "chat" {
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				} else {
					_, err = svc.ForwardAsResponses(context.Background(), c, account, body, nil)
				}
				require.Error(t, err)
				require.Equal(t, 1, repo.calls)
				if rotated {
					require.Equal(t, []string{"Bearer old", "Bearer new"}, up.tokens)
				} else {
					require.Equal(t, []string{"Bearer old"}, up.tokens)
				}
			})
		}
	}
}
func TestV0222ClaudeRotationDoesNotRefreshOtherAccountsOrExpiredAttempts(t *testing.T) {
	repo := &v0222RotatedRepo{latest: &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "new"}}}
	svc := &GatewayService{accountRepo: repo}
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	require.Empty(t, svc.rotatedClaudeAccessToken(context.Background(), account, "old"))
	require.Zero(t, repo.calls)
	account.Type = AccountTypeOAuth
	require.Empty(t, svc.rotatedClaudeAccessToken(context.Background(), account, "old", time.Now().Add(-time.Second)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := repo.calls
	require.Empty(t, svc.rotatedClaudeAccessToken(ctx, account, "old"))
	require.Equal(t, calls, repo.calls)
}
