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
	anthropicPoolRecoveryProbeMaxBackoff     = 30 * time.Second
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
	if s == nil || account == nil || !isAnthropicAPIKeyPoolAccount(account) {
		return
	}
	cooldownUntil, ok := s.anthropicPoolAccountSoftCooldownUntil(account)
	if !ok || time.Now().Before(cooldownUntil) {
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
		s.runAnthropicPoolRecoveryProbe(probeCtx, &accountCopy, requestedModel, cooldownUntil)
	}()
}

func (s *GatewayService) clearAnthropicPoolSoftCooldownIfRecoveryProbeDisabled(ctx context.Context, account *Account, requestedModel string) bool {
	if s == nil || account == nil || !isAnthropicAPIKeyPoolAccount(account) {
		return false
	}
	cooldownUntil, ok := s.anthropicPoolAccountSoftCooldownUntil(account)
	if !ok || time.Now().Before(cooldownUntil) {
		return false
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
	if s == nil || account == nil || !isAnthropicAPIKeyPoolAccount(account) {
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

func (s *GatewayService) runAnthropicPoolRecoveryProbe(ctx context.Context, account *Account, requestedModel string, cooldownUntil time.Time) {
	result := s.probeAnthropicPoolAccountRecovery(ctx, account, requestedModel)
	if !s.anthropicPoolAccountSoftCooldownMatches(account.ID, cooldownUntil) {
		loggerLegacyAnthropicPoolRecovery("probe_result_ignored_stale account_id=%d endpoint=%s status=%d", account.ID, result.endpoint, result.statusCode)
		return
	}
	if result.success {
		s.clearAnthropicPoolSoftCooldown(account.ID)
		s.anthropicPoolRecoveryProbeFailureCnt.Delete(account.ID)
		if s.rateLimitService != nil {
			if _, err := s.rateLimitService.RecoverAccountAfterSuccessfulTest(ctx, account.ID); err != nil {
				loggerLegacyAnthropicPoolRecovery("recover_state_failed account_id=%d err=%v", account.ID, err)
			}
		}
		loggerLegacyAnthropicPoolRecovery("probe_success account_id=%d endpoint=%s status=%d", account.ID, result.endpoint, result.statusCode)
		return
	}

	backoff := s.nextAnthropicPoolRecoveryProbeBackoff(account.ID, result.retryable)
	until := time.Now().Add(backoff)
	s.storeAnthropicPoolSoftCooldownUntil(account.ID, until)
	s.storeAnthropicPoolRecoveryProbeBackoffContext(ctx, account.ID, result)
	if result.err != nil {
		loggerLegacyAnthropicPoolRecovery("probe_failed account_id=%d endpoint=%s status=%d backoff=%s err=%v", account.ID, result.endpoint, result.statusCode, backoff, result.err)
	} else {
		loggerLegacyAnthropicPoolRecovery("probe_failed account_id=%d endpoint=%s status=%d backoff=%s", account.ID, result.endpoint, result.statusCode, backoff)
	}
}

func (s *GatewayService) probeAnthropicPoolAccountRecovery(ctx context.Context, account *Account, requestedModel string) anthropicPoolRecoveryProbeResult {
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
	req.Header.Set("x-api-key", token)
	req.Header.Set("anthropic-version", "2023-06-01")
	return s.doAnthropicPoolRecoveryProbe(req, account, "messages")
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

func (s *GatewayService) nextAnthropicPoolRecoveryProbeBackoff(accountID int64, retryable bool) time.Duration {
	if !retryable {
		return anthropicPoolSoftCooldownAuth
	}
	failures := 1
	if current, ok := s.anthropicPoolRecoveryProbeFailureCnt.Load(accountID); ok {
		if count, ok := current.(int); ok && count > 0 {
			failures = count + 1
		}
	}
	s.anthropicPoolRecoveryProbeFailureCnt.Store(accountID, failures)
	backoff := anthropicPoolRecoveryProbeDefaultBackoff
	for i := 1; i < failures; i++ {
		backoff *= 2
		if backoff >= anthropicPoolRecoveryProbeMaxBackoff {
			return anthropicPoolRecoveryProbeMaxBackoff
		}
	}
	return backoff
}

func (s *GatewayService) storeAnthropicPoolRecoveryProbeBackoffContext(ctx context.Context, accountID int64, result anthropicPoolRecoveryProbeResult) {
	cooldownContext := s.anthropicPoolAccountSoftCooldownContext(accountID)
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
