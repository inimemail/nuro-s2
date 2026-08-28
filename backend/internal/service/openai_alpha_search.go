package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	chatgptCodexAlphaSearchURL   = "https://chatgpt.com/backend-api/codex/alpha/search"
	openAIPlatformAlphaSearchURL = "https://api.openai.com/v1/alpha/search"
)

// ForwardAlphaSearch forwards the evolving alpha schema as opaque JSON. Only
// a 2xx response returns a billable result.
func (s *OpenAIGatewayService) ForwardAlphaSearch(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	if s == nil || c == nil || account == nil {
		return nil, fmt.Errorf("service, context, and account are required")
	}
	modelResult := gjson.GetBytes(body, "model")
	requestedModel := strings.TrimSpace(modelResult.String())
	if modelResult.Type != gjson.String || requestedModel == "" {
		return nil, fmt.Errorf("model is required")
	}
	upstreamModel := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(requestedModel))
	if upstreamModel != "" && upstreamModel != requestedModel {
		body = ReplaceModelInBody(body, upstreamModel)
	}
	attempt := newOpenAIUpstreamAttempt()
	attemptCtx := withOpenAIUpstreamAttempt(ctx, attempt)
	token, _, err := s.GetAccessToken(attemptCtx, account)
	if err != nil {
		return nil, err
	}
	req, err := s.buildOpenAIAlphaSearchRequest(attemptCtx, c, account, body, token)
	if err != nil {
		// A custom account can be syntactically schedulable while the runtime URL
		// trust policy rejects its host. Treat that as request-local ineligibility
		// so one bad account cannot terminate a mixed-pool search request, and do
		// not expose the rejected URL or validator diagnostics downstream.
		// This is a configuration/build failure, not a transport failure. In race
		// mode it must not consume the separate transport retry allowance.
		return nil, newOpenAIAlphaSearchFailoverErrorForAccount(account, http.StatusBadGateway, nil, safeUpstreamErrorMessage, false, false)
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	upstreamStart := time.Now()
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.resolveTLSProfile(account))
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		// The request reached the transport layer and failed before an HTTP
		// response was available, so this is eligible for the race transport rule.
		failoverErr := annotateOpenAIAttemptFailover(req, newOpenAIAlphaSearchFailoverErrorForAccount(account, http.StatusBadGateway, nil, safeUpstreamErrorMessage, false, true))
		failoverErr.RetryableOnSameAccount = false
		return nil, failoverErr
	}
	attempt.markResponse(resp.StatusCode, strings.TrimSpace(firstNonEmptyString(resp.Header.Get("x-request-id"), resp.Header.Get("request-id"))))
	defer func() { _ = resp.Body.Close() }()
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, annotateOpenAIAttemptFailover(req, newOpenAIAlphaSearchFailoverErrorForAccount(account, http.StatusBadGateway, nil, safeUpstreamErrorMessage, false, true))
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		upstreamMessage := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
		if upstreamMessage == "" {
			upstreamMessage = safeUpstreamErrorMessage
		}
		retrySameAccount := openAIPoolFailoverRetryableOnSameAccount(account, resp.StatusCode, upstreamMessage, respBody)
		failoverErr := annotateOpenAIAttemptFailover(req, newOpenAIAlphaSearchFailoverErrorForAccount(account, resp.StatusCode, respBody, upstreamMessage, retrySameAccount, false))
		// Alpha Search deliberately bypasses same-account retries; preserve its
		// endpoint-specific failover contract while retaining Attempt metadata.
		failoverErr.RetryableOnSameAccount = false
		return nil, failoverErr
	}
	if !openAIAlphaSearchSuccessResponseIsValid(respBody) {
		return nil, annotateOpenAIAttemptFailover(req, newOpenAIAlphaSearchFailoverErrorForAccount(
			account,
			http.StatusBadGateway,
			openAIUpstreamFailoverErrorBody(safeUpstreamErrorMessage),
			safeUpstreamErrorMessage,
			false,
			true,
		))
	}
	if !account.IsShadow() {
		s.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, resp.Header)
	}
	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := responseheaders.SafeContentType(resp.Header.Get("Content-Type"), "application/json")
	c.Data(resp.StatusCode, contentType, respBody)
	return &OpenAIForwardResult{
		RequestID:                  strings.TrimSpace(resp.Header.Get("x-request-id")),
		AttemptID:                  attempt.ID,
		UpstreamRequestBodyStarted: OpenAIUpstreamAttemptBodyStarted(attemptCtx),
		Model:                      requestedModel,
		UpstreamModel:              upstreamModel,
		Duration:                   time.Since(upstreamStart),
		WebSearchCalls:             1,
	}, nil
}

func openAIAlphaSearchSuccessResponseIsValid(body []byte) bool {
	if openAIPassthroughResponseIsUnsafe(body) {
		return false
	}
	var payload map[string]json.RawMessage
	return json.Unmarshal(body, &payload) == nil && len(payload) > 0
}

func newOpenAIAlphaSearchFailoverError(statusCode int, body []byte, message string, retrySameAccount bool) *UpstreamFailoverError {
	return &UpstreamFailoverError{
		StatusCode:                statusCode,
		ResponseBody:              append([]byte(nil), body...),
		Message:                   message,
		RetryableOnSameAccount:    retrySameAccount,
		SkipPoolSoftCooldown:      true,
		SkipPromptCacheAvoidance:  true,
		SkipStickySessionEviction: true,
		SkipSchedulePenalty:       true,
	}
}

func newOpenAIAlphaSearchFailoverErrorForAccount(
	account *Account,
	statusCode int,
	body []byte,
	message string,
	retrySameAccount bool,
	transportFailure bool,
) *UpstreamFailoverError {
	failoverErr := newOpenAIAlphaSearchFailoverError(statusCode, body, message, retrySameAccount)
	if account == nil || !account.IsOpenAIUpstreamConcurrencyRaceEnabled() {
		return failoverErr
	}
	if !transportFailure && !retrySameAccount {
		return failoverErr
	}
	ruleStatus := statusCode
	if transportFailure {
		ruleStatus = 0
	}
	key, limit, matched := account.OpenAIUpstreamConcurrencyRaceRetryRule(ruleStatus)
	failoverErr.RetryRuleKey = key
	failoverErr.RetryRuleLimit = limit
	failoverErr.RetryRuleTransport = transportFailure && key == "transport"
	failoverErr.RetryableOnSameAccount = matched && key != "" && limit > 0
	return failoverErr
}

func (s *OpenAIGatewayService) buildOpenAIAlphaSearchRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string) (*http.Request, error) {
	clientBeta := c.GetHeader("OpenAI-Beta")
	req, err := s.buildUpstreamRequestOpenAIPassthrough(ctx, c, account, body, token)
	if err != nil {
		return nil, err
	}
	targetURL, err := s.openAIAlphaSearchURL(account)
	if err != nil {
		return nil, err
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("parse alpha search URL: %w", err)
	}
	if c.Request != nil && c.Request.URL != nil {
		query := parsedURL.Query()
		for key, values := range c.Request.URL.Query() {
			for _, value := range values {
				query.Add(key, value)
			}
		}
		parsedURL.RawQuery = query.Encode()
	}
	req.URL = parsedURL
	req.Header.Set("Accept", "application/json")
	if clientBeta == "" {
		req.Header.Del("OpenAI-Beta")
	}
	if version := strings.TrimSpace(c.GetHeader("Version")); version != "" {
		req.Header.Set("Version", version)
	} else if account.Type == AccountTypeOAuth {
		req.Header.Set("Version", codexCLIVersion)
	}
	return req, nil
}

func (s *OpenAIGatewayService) openAIAlphaSearchURL(account *Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("account is required")
	}
	if !IsOpenAIAlphaSearchAccountEligible(account) {
		return "", fmt.Errorf("alpha search account configuration is invalid")
	}
	switch account.Type {
	case AccountTypeOAuth:
		return chatgptCodexAlphaSearchURL, nil
	case AccountTypeAPIKey:
		baseURL := account.GetOpenAIBaseURL()
		if baseURL == "" {
			return openAIPlatformAlphaSearchURL, nil
		}
		validatedURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return "", err
		}
		return buildOpenAIEndpointURL(validatedURL, "/v1/alpha/search"), nil
	default:
		return "", fmt.Errorf("unsupported OpenAI account type: %s", account.Type)
	}
}

func IsOpenAIAlphaSearchAccountEligible(account *Account) bool {
	if account == nil || !account.IsOpenAI() {
		return false
	}
	if account.Type == AccountTypeOAuth {
		return true
	}
	if account.Type != AccountTypeAPIKey {
		return false
	}
	baseURL := strings.TrimSpace(account.GetOpenAIBaseURL())
	if baseURL == "" {
		return true
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed == nil || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	// Outbound host/IP/DNS policy is enforced immediately before request build by
	// validateUpstreamBaseURL. Keep the scheduler check side-effect free while
	// allowing configured OpenAI-compatible API-key upstreams to participate.
	return (strings.EqualFold(parsed.Scheme, "https") || strings.EqualFold(parsed.Scheme, "http")) && strings.TrimSpace(parsed.Hostname()) != ""
}
