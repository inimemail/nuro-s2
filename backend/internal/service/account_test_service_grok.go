package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *AccountTestService) testGrokAccountConnection(c *gin.Context, account *Account, modelID string, prompt string) error {
	ctx := c.Request.Context()
	if s.httpUpstream == nil {
		return s.sendErrorAndEnd(c, "HTTP upstream not configured")
	}
	model := strings.TrimSpace(modelID)
	if model == "" {
		model = "grok-4.5"
	}
	model = account.GetMappedModel(model)
	var token string
	var err error
	switch account.Type {
	case AccountTypeOAuth:
		if s.grokTokenProvider == nil {
			return s.sendErrorAndEnd(c, "Grok token provider not configured")
		}
		token, err = s.grokTokenProvider.GetAccessTokenForManualTest(ctx, account)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to get Grok access token")
		}
	case AccountTypeAPIKey:
		token = strings.TrimSpace(account.GetGrokAccessToken())
		if token == "" {
			return s.sendErrorAndEnd(c, "Grok API key is missing")
		}
	default:
		return s.sendErrorAndEnd(c, fmt.Sprintf("Unsupported Grok account type: %s", account.Type))
	}
	body, err := json.Marshal(map[string]any{
		"model":  model,
		"input":  []map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": firstNonEmptyTestPrompt(prompt, "hi")}}}},
		"stream": true,
	})
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok test payload")
	}
	url, err := buildGrokResponsesURL(account, s.cfg)
	if err != nil {
		return s.sendErrorAndEnd(c, "Invalid Grok base URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return s.sendErrorAndEnd(c, "Failed to create Grok request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	applyGrokOAuthIdentityHeaders(req.Header, url, account.IsGrokOAuth())
	account.ApplyHeaderOverrides(req.Header)
	if account.Proxy != nil {
		resp, err := s.httpUpstream.DoWithTLS(req, account.Proxy.URL(), account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
		return s.finishGrokTest(c, resp, err, model)
	}
	resp, err := s.httpUpstream.DoWithTLS(req, "", account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	return s.finishGrokTest(c, resp, err, model)
}

func firstNonEmptyTestPrompt(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func (s *AccountTestService) finishGrokTest(c *gin.Context, resp *http.Response, err error, model string) error {
	if err != nil {
		return s.sendErrorAndEnd(c, "Grok request failed")
	}
	if resp == nil {
		return s.sendErrorAndEnd(c, "Grok returned no response")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Grok API returned %d", resp.StatusCode))
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	return s.processOpenAIStream(c, resp.Body)
}
