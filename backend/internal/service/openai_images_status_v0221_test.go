package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type pendingImageUpstream struct {
	started chan struct{}
	reply   chan *http.Response
}

func (u *pendingImageUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	close(u.started)
	select {
	case resp := <-u.reply:
		return resp, nil
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

func (u *pendingImageUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestImagesNonStreamPendingDoesNotCommitSuccessBeforeError(t *testing.T) {
	setGinTestMode()
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError, 0} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			body := []byte(`{"model":"gpt-image-2","prompt":"fixture"}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("X-Codex-Image-Turn-Id", "fixture-turn")
			upstream := &pendingImageUpstream{started: make(chan struct{}), reply: make(chan *http.Response, 1)}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			account := &Account{ID: 81, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "sk-fixture"}}
			result := make(chan error, 1)
			go func() {
				_, err := svc.ForwardImages(ctx, c, account, body, parsed, "")
				result <- err
			}()
			select {
			case <-upstream.started:
			case <-ctx.Done():
				t.Fatal("image request did not reach the controlled upstream")
			}
			require.False(t, c.Writer.Written(), "a pending non-stream request must not commit HTTP 200")
			if status == 0 {
				cancel()
			} else {
				upstream.reply <- &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"private.invalid token-secret"}}`))}
			}
			select {
			case err = <-result:
			case <-time.After(3 * time.Second):
				t.Fatal("image request did not stop after its response/cancellation")
			}
			require.Error(t, err)
			var failover *UpstreamFailoverError
			if errors.As(err, &failover) {
				require.Equal(t, status, failover.StatusCode)
				require.False(t, c.Writer.Written(), "the handler can still choose the final error status")
			} else if status != 0 {
				require.Equal(t, status, rec.Code)
			}
			require.NotContains(t, rec.Body.String(), "private.invalid")
			require.NotContains(t, rec.Body.String(), "token-secret")
		})
	}
}
