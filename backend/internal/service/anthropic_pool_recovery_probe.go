package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	anthropicPoolRecoveryProbeDefaultTimeout = 5 * time.Second
	anthropicPoolRecoveryProbeDefaultBackoff = 5 * time.Second
	anthropicPoolRecoveryProbeAdminKickEvery = 5 * time.Second
	anthropicPoolRecoveryProbeReadLimit      = 1 << 20
)

type anthropicPoolRecoveryProbeResult struct {
	success        bool
	retryable      bool
	statusCode     int
	endpoint       string
	responseHeader http.Header
	err            error
	message        string
}

func (s *GatewayService) maybeStartAnthropicPoolRecoveryProbe(ctx context.Context, account *Account, requestedModel string) {
	if s == nil || account == nil || !isAnthropicPoolAccount(account) {
		return
	}
	if !account.IsActive() || !account.Schedulable {
		s.clearAnthropicPoolSoftCooldown(account.ID)
		return
	}
	if !account.IsPoolSoftCooldownEnabled() {
		s.clearAnthropicPoolSoftCooldown(account.ID)
		return
	}
	cooldownUntil, ok := s.anthropicPoolAccountSoftCooldownUntil(account)
	if !ok || time.Now().Before(cooldownUntil) {
		return
	}
	clearGeneration, ok := accountRuntimeDeadlineGeneration(&s.anthropicPoolSoftCooldownUntil, account.ID, cooldownUntil)
	if !ok {
		return
	}
	if s.clearAnthropicPoolSoftCooldownIfRecoveryProbeDisabled(ctx, account, requestedModel) {
		return
	}
	if s.httpUpstream == nil {
		return
	}
	if _, loaded := s.anthropicPoolRecoveryProbeInFlight.LoadOrStore(account.ID, struct{}{}); loaded {
		return
	}
	accountID := account.ID
	accountCopy := *account
	timeout := s.anthropicPoolRecoveryProbeTimeout(ctx)
	go func() {
		defer s.anthropicPoolRecoveryProbeInFlight.Delete(accountID)
		probeCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		s.runAnthropicPoolRecoveryProbe(probeCtx, &accountCopy, requestedModel, cooldownUntil, clearGeneration)
	}()
}

func (s *GatewayService) clearAnthropicPoolSoftCooldownIfRecoveryProbeDisabled(ctx context.Context, account *Account, requestedModel string) bool {
	if s == nil || account == nil || !isAnthropicPoolAccount(account) {
		return false
	}
	cooldownUntil, ok := s.anthropicPoolAccountSoftCooldownUntil(account)
	if !ok || time.Now().Before(cooldownUntil) {
		return false
	}
	if !account.IsPoolSoftCooldownEnabled() {
		s.clearAnthropicPoolSoftCooldown(account.ID)
		loggerLegacyAnthropicPoolRecovery("account_soft_cooldown_disabled_recover account_id=%d model=%s", account.ID, requestedModel)
		return true
	}
	if s.anthropicPoolRecoveryProbeEnabled(ctx) {
		return false
	}
	s.clearAnthropicPoolSoftCooldown(account.ID)
	loggerLegacyAnthropicPoolRecovery("probe_disabled_recover account_id=%d model=%s", account.ID, requestedModel)
	return true
}

func (s *GatewayService) anthropicPoolRecoveryProbeEnabled(ctx context.Context) bool {
	if s == nil || s.settingService == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return s.settingService.IsAnthropicPoolRecoveryProbeEnabled(ctx)
}

func (s *GatewayService) anthropicPoolRecoveryProbeModel(ctx context.Context) string {
	if s != nil && s.settingService != nil {
		if model := strings.TrimSpace(s.settingService.GetAnthropicPoolRecoveryProbeModel(ctx)); model != "" {
			return model
		}
	}
	return anthropicPoolProbeDefaultModel
}

func (s *GatewayService) anthropicPoolRecoveryProbeTimeout(ctx context.Context) time.Duration {
	if s != nil && s.settingService != nil {
		if timeout := s.settingService.GetAnthropicPoolProbeTimeout(ctx); timeout > 0 {
			return timeout
		}
	}
	return anthropicPoolRecoveryProbeDefaultTimeout
}

func (s *GatewayService) MaybeKickAnthropicPoolRecoveryProbeFromAdminList(ctx context.Context, account *Account) {
	if s == nil || account == nil || !isAnthropicPoolAccount(account) {
		return
	}
	if !account.IsActive() || !account.Schedulable {
		s.clearAnthropicPoolSoftCooldown(account.ID)
		return
	}
	cooldownUntil, ok := s.anthropicPoolAccountSoftCooldownUntil(account)
	if !ok || time.Now().Before(cooldownUntil) {
		return
	}
	if _, probing := s.anthropicPoolRecoveryProbeInFlight.Load(account.ID); probing {
		return
	}
	now := time.Now()
	if value, ok := s.anthropicPoolRecoveryProbeAdminKick.Load(account.ID); ok {
		if last, ok := value.(time.Time); ok && now.Sub(last) < anthropicPoolRecoveryProbeAdminKickEvery {
			return
		}
	}
	s.anthropicPoolRecoveryProbeAdminKick.Store(account.ID, now)
	cooldownContext := s.anthropicPoolAccountSoftCooldownContext(account.ID)
	s.maybeStartAnthropicPoolRecoveryProbe(ctx, account, cooldownContext.ProbeModel)
}

func (s *GatewayService) runAnthropicPoolRecoveryProbe(ctx context.Context, account *Account, requestedModel string, cooldownUntil time.Time, generations ...int64) {
	clearGeneration := int64(0)
	if len(generations) > 0 {
		clearGeneration = generations[0]
	} else {
		var ok bool
		clearGeneration, ok = accountRuntimeDeadlineGeneration(&s.anthropicPoolSoftCooldownUntil, account.ID, cooldownUntil)
		if !ok {
			return
		}
	}
	result := s.probeAnthropicPoolAccountRecovery(ctx, account, requestedModel)
	if result.success {
		if !deleteAccountRuntimeDeadlineIfMatches(&s.anthropicPoolSoftCooldownUntil, account.ID, cooldownUntil, clearGeneration) {
			loggerLegacyAnthropicPoolRecovery("probe_result_ignored_stale account_id=%d endpoint=%s status=%d", account.ID, result.endpoint, result.statusCode)
			return
		}
		s.clearAnthropicPoolSoftCooldownBefore(account.ID, clearGeneration+1)
		s.anthropicPoolRecoveryProbeFailureCnt.Delete(account.ID)
		if s.rateLimitService != nil {
			if _, err := s.rateLimitService.RecoverAccountAfterSuccessfulTest(ctx, account.ID); err != nil {
				loggerLegacyAnthropicPoolRecovery("recover_state_failed account_id=%d err=%v", account.ID, err)
			}
		}
		loggerLegacyAnthropicPoolRecovery("probe_success account_id=%d endpoint=%s status=%d", account.ID, result.endpoint, result.statusCode)
		return
	}

	backoff := s.nextAnthropicPoolRecoveryProbeBackoff(ctx, account, result.retryable)
	until := time.Now().Add(backoff)
	if !replaceAccountRuntimeDeadlineIfMatches(&s.anthropicPoolSoftCooldownUntil, account.ID, cooldownUntil, clearGeneration, until) {
		loggerLegacyAnthropicPoolRecovery("probe_result_ignored_stale account_id=%d endpoint=%s status=%d", account.ID, result.endpoint, result.statusCode)
		return
	}
	s.storeAnthropicPoolRecoveryProbeBackoffContext(ctx, account.ID, result, clearGeneration)
	if latestGeneration := s.currentAccountRuntimeClearGeneration(account.ID); latestGeneration > clearGeneration {
		s.clearAnthropicPoolSoftCooldownBefore(account.ID, latestGeneration)
	}
	if result.err != nil {
		loggerLegacyAnthropicPoolRecovery("probe_failed account_id=%d endpoint=%s status=%d backoff=%s err=%v", account.ID, result.endpoint, result.statusCode, backoff, result.err)
	} else {
		loggerLegacyAnthropicPoolRecovery("probe_failed account_id=%d endpoint=%s status=%d backoff=%s", account.ID, result.endpoint, result.statusCode, backoff)
	}
}

func (s *GatewayService) probeAnthropicPoolAccountRecovery(ctx context.Context, account *Account, requestedModel string) anthropicPoolRecoveryProbeResult {
	if account != nil && account.IsBedrock() {
		return s.probeBedrockPoolAccountRecovery(ctx, account, requestedModel)
	}
	model := s.resolveAnthropicPoolRecoveryProbeModel(ctx, account, requestedModel)
	token := strings.TrimSpace(account.GetCredential("api_key"))
	if token == "" {
		return anthropicPoolRecoveryProbeResult{retryable: false, endpoint: "messages", statusCode: http.StatusUnauthorized, err: fmt.Errorf("api_key not found")}
	}
	baseURL := strings.TrimSpace(account.GetBaseURL())
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return anthropicPoolRecoveryProbeResult{retryable: true, endpoint: "messages", err: err}
	}
	body := anthropicPoolRecoveryMessagesPayload(model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(validatedURL, "/")+"/v1/messages?beta=true", bytes.NewReader(body))
	if err != nil {
		return anthropicPoolRecoveryProbeResult{retryable: true, endpoint: "messages", err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	setAnthropicAPIKeyAuthHeader(req.Header, account, token)
	req.Header.Set("anthropic-version", "2023-06-01")
	return s.doAnthropicPoolRecoveryProbe(req, account, "messages")
}

func (s *GatewayService) probeBedrockPoolAccountRecovery(ctx context.Context, account *Account, requestedModel string) anthropicPoolRecoveryProbeResult {
	model := s.resolveAnthropicPoolRecoveryProbeModel(ctx, account, requestedModel)
	modelID, ok := ResolveBedrockModelID(account, model)
	if !ok {
		return anthropicPoolRecoveryProbeResult{
			retryable:  false,
			endpoint:   "bedrock",
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("unsupported bedrock probe model: %s", model),
			message:    "unsupported bedrock probe model",
		}
	}
	body, err := PrepareBedrockRequestBodyWithTokens(anthropicPoolRecoveryMessagesPayload(model), modelID, nil, false)
	if err != nil {
		return anthropicPoolRecoveryProbeResult{retryable: false, endpoint: "bedrock", statusCode: http.StatusBadRequest, err: err}
	}

	region := bedrockRuntimeRegion(account)
	var req *http.Request
	if account.IsBedrockAPIKey() {
		apiKey := strings.TrimSpace(account.GetCredential("api_key"))
		if apiKey == "" {
			return anthropicPoolRecoveryProbeResult{retryable: false, endpoint: "bedrock", statusCode: http.StatusUnauthorized, err: fmt.Errorf("api_key not found")}
		}
		req, err = s.buildUpstreamRequestBedrockAPIKey(ctx, body, modelID, region, false, apiKey)
	} else {
		var signer *BedrockSigner
		signer, err = NewBedrockSignerFromAccount(account)
		if err == nil {
			req, err = s.buildUpstreamRequestBedrock(ctx, body, modelID, region, false, signer)
		}
	}
	if err != nil {
		return anthropicPoolRecoveryProbeResult{retryable: false, endpoint: "bedrock", statusCode: http.StatusUnauthorized, err: err}
	}
	return s.doBedrockPoolRecoveryProbe(req, account)
}

func (s *GatewayService) resolveAnthropicPoolRecoveryProbeModel(ctx context.Context, account *Account, requestedModel string) string {
	cooldownContext := s.anthropicPoolAccountSoftCooldownContext(account.ID)
	for _, value := range []string{requestedModel, cooldownContext.ProbeModel, s.anthropicPoolRecoveryProbeModel(ctx), anthropicPoolProbeDefaultModel} {
		if model := strings.TrimSpace(value); model != "" {
			if mapped := account.GetMappedModel(model); strings.TrimSpace(mapped) != "" {
				return mapped
			}
			return model
		}
	}
	return anthropicPoolProbeDefaultModel
}

func (s *GatewayService) doBedrockPoolRecoveryProbe(req *http.Request, account *Account) anthropicPoolRecoveryProbeResult {
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, nil)
	if err != nil {
		return anthropicPoolRecoveryProbeResult{retryable: true, endpoint: "bedrock", err: err}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, anthropicPoolRecoveryProbeReadLimit))
	msg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if readErr != nil {
		return anthropicPoolRecoveryProbeResult{
			retryable:      true,
			statusCode:     resp.StatusCode,
			endpoint:       "bedrock",
			responseHeader: resp.Header,
			err:            readErr,
			message:        msg,
		}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return anthropicPoolRecoveryProbeResult{success: true, statusCode: resp.StatusCode, endpoint: "bedrock", responseHeader: resp.Header}
	}
	if msg == "" {
		msg = truncateString(strings.TrimSpace(string(body)), 256)
	}
	return anthropicPoolRecoveryProbeResult{
		retryable:      anthropicPoolRecoveryProbeStatusRetryable(resp.StatusCode),
		statusCode:     resp.StatusCode,
		endpoint:       "bedrock",
		responseHeader: resp.Header,
		message:        msg,
	}
}

func (s *GatewayService) doAnthropicPoolRecoveryProbe(req *http.Request, account *Account, endpoint string) anthropicPoolRecoveryProbeResult {
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if err != nil {
		return anthropicPoolRecoveryProbeResult{retryable: true, endpoint: endpoint, err: err}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, anthropicPoolRecoveryProbeReadLimit))
	msg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if readErr != nil {
		return anthropicPoolRecoveryProbeResult{
			retryable:      true,
			statusCode:     resp.StatusCode,
			endpoint:       endpoint,
			responseHeader: resp.Header,
			err:            readErr,
			message:        msg,
		}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return anthropicPoolRecoveryProbeResult{success: true, statusCode: resp.StatusCode, endpoint: endpoint, responseHeader: resp.Header}
	}
	if msg == "" {
		msg = truncateString(strings.TrimSpace(string(body)), 256)
	}
	return anthropicPoolRecoveryProbeResult{
		retryable:      anthropicPoolRecoveryProbeStatusRetryable(resp.StatusCode),
		statusCode:     resp.StatusCode,
		endpoint:       endpoint,
		responseHeader: resp.Header,
		message:        msg,
	}
}

func anthropicPoolRecoveryMessagesPayload(model string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
	})
	return payload
}

func anthropicPoolRecoveryProbeStatusRetryable(statusCode int) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return false
	default:
		return true
	}
}

func (s *GatewayService) nextAnthropicPoolRecoveryProbeBackoff(ctx context.Context, account *Account, retryable bool) time.Duration {
	if account == nil {
		return anthropicPoolRecoveryProbeDefaultBackoff
	}
	failures := 1
	if current, ok := s.anthropicPoolRecoveryProbeFailureCnt.Load(account.ID); ok {
		if count, ok := current.(int); ok && count > 0 {
			failures = count + 1
		}
	}
	s.anthropicPoolRecoveryProbeFailureCnt.Store(account.ID, failures)
	backoff := s.configuredAnthropicPoolSoftCooldownMax(ctx)
	if backoff <= 0 {
		if !retryable {
			return anthropicPoolSoftCooldownAuth
		}
		return anthropicPoolRecoveryProbeDefaultBackoff
	}
	return backoff
}

func (s *GatewayService) storeAnthropicPoolRecoveryProbeBackoffContext(ctx context.Context, accountID int64, result anthropicPoolRecoveryProbeResult, generations ...int64) {
	cooldownContext := s.anthropicPoolAccountSoftCooldownContext(accountID)
	if len(generations) > 0 {
		cooldownContext.ClearGeneration = generations[0]
	} else {
		cooldownContext.ClearGeneration = s.currentAccountRuntimeClearGeneration(accountID)
	}
	cooldownContext.LastProbeStatus = result.statusCode
	cooldownContext.LastProbeReason = truncateString(strings.TrimSpace(firstNonEmptyString(result.message, result.err)), 256)
	if isAnthropicPoolProbeModelError(result.statusCode, result.message) {
		cooldownContext.ProbeModel = s.anthropicPoolRecoveryProbeModel(ctx)
	}
	s.anthropicPoolSoftCooldownContext.Store(accountID, cooldownContext)
}

func loggerLegacyAnthropicPoolRecovery(format string, args ...any) {
	logger.LegacyPrintf("service.anthropic_pool_recovery", format, args...)
}
