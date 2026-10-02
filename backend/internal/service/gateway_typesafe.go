package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/typesafe"
	"github.com/gin-gonic/gin"
)

// ForwardSystemOne forwards a native TypeSafe System One request without
// passing through any Anthropic/OpenAI request transformation.
func (s *GatewayService) ForwardSystemOne(ctx context.Context, account *Account, body []byte) (*ForwardResult, []byte, int, error) {
	if s == nil || s.httpUpstream == nil || account == nil || !account.IsTypeSafe() {
		return nil, nil, 0, errors.New("typesafe account is unavailable")
	}
	if account.Type != AccountTypeAPIKey {
		return nil, nil, http.StatusBadRequest, errors.New("typesafe accounts require an API key")
	}
	key := account.GetTypeSafeAPIKey()
	if key == "" {
		return nil, nil, http.StatusUnauthorized, errors.New("typesafe API key is missing")
	}
	req, err := typesafe.NewSystemOneRequest(ctx, account.GetTypeSafeBaseURL(), key, body)
	if err != nil {
		return nil, nil, http.StatusBadGateway, err
	}
	account.ApplyHeaderOverrides(req.Header)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	started := time.Now()
	var resp *http.Response
	if s.tlsFPProfileService != nil {
		resp, err = s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	} else {
		resp, err = s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	}
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, http.StatusBadGateway, errors.New("typesafe upstream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, resp.StatusCode, fmt.Errorf("typesafe upstream returned status %d", resp.StatusCode)
	}
	decoded, err := typesafe.DecodeSystemOneResponse(resp.Body)
	if err != nil {
		return nil, nil, http.StatusBadGateway, err
	}
	result := &ForwardResult{Model: typesafe.JevLatestModel, UpstreamModel: typesafe.JevLatestModel, UpstreamResponseModel: strings.TrimSpace(decoded.Model), Duration: time.Since(started), Usage: ClaudeUsage{InputTokens: decoded.Usage.InputTokens, OutputTokens: decoded.Usage.OutputTokens}}
	return result, decoded.Body, resp.StatusCode, nil
}

// writeTypeSafeError is kept in the service package for unit tests that call
// the native forwarding helper through a gin handler.
func writeTypeSafeError(c *gin.Context, status int, message string) {
	if c == nil {
		return
	}
	c.JSON(status, gin.H{"error": gin.H{"type": "typesafe_error", "message": message}})
}
