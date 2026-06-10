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
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
)

const (
	openAIPoolRecoveryProbeTimeout           = 5 * time.Second
	openAIPoolRecoveryProbeImageTimeout      = 6 * time.Minute
	openAIPoolRecoveryProbeDefaultImageModel = "gpt-image-2"
	openAIPoolRecoveryProbeDefaultBackoff    = 5 * time.Second
	openAIPoolRecoveryProbeMaxBackoff        = 30 * time.Second
	openAIPoolRecoveryProbeAdminKickEvery    = 5 * time.Second
	openAIPoolRecoveryProbeReadLimit         = 1 << 20
	openAIPoolRecoveryProbeMaxOutputTokens   = 8
)

type openAIPoolRecoveryProbeResult struct {
	success        bool
	retryable      bool
	statusCode     int
	endpoint       string
	responseHeader http.Header
	err            error
}

func (s *OpenAIGatewayService) maybeStartOpenAIPoolRecoveryProbe(ctx context.Context, account *Account, requestedModel string) {
	if s == nil || account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return
	}
	cooldownUntil, ok := s.openAIPoolAccountSoftCooldownUntil(account)
	if !ok || time.Now().Before(cooldownUntil) {
		return
	}
	if s.clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(ctx, account, requestedModel) {
		return
	}
	if s.httpUpstream == nil {
		return
	}
	if _, loaded := s.openaiPoolRecoveryProbeInFlight.LoadOrStore(account.ID, struct{}{}); loaded {
		return
	}
	accountID := account.ID
	accountCopy := *account
	probeTimeout := s.openAIPoolRecoveryProbeTimeout(ctx, &accountCopy, requestedModel)
	go func() {
		defer s.openaiPoolRecoveryProbeInFlight.Delete(accountID)
		probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		s.runOpenAIPoolRecoveryProbe(probeCtx, &accountCopy, requestedModel, cooldownUntil)
	}()
}

func (s *OpenAIGatewayService) clearOpenAIPoolSoftCooldownIfRecoveryProbeDisabled(ctx context.Context, account *Account, requestedModel string) bool {
	if s == nil || account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return false
	}
	cooldownUntil, ok := s.openAIPoolAccountSoftCooldownUntil(account)
	if !ok || time.Now().Before(cooldownUntil) {
		return false
	}
	if s.openAIPoolRecoveryProbeEnabled(ctx, account, requestedModel) {
		return false
	}
	usesImagePool := s.openAIPoolRecoveryProbeUsesImagePool(account, requestedModel)
	s.ClearAccountSchedulingBlock(account.ID)
	loggerLegacyOpenAIPoolRecovery("probe_disabled_recover account_id=%d image_pool=%t", account.ID, usesImagePool)
	return true
}

func (s *OpenAIGatewayService) openAIPoolRecoveryProbeEnabled(ctx context.Context, account *Account, requestedModel string) bool {
	if s == nil || s.settingService == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.openAIPoolRecoveryProbeUsesImagePool(account, requestedModel) {
		return s.settingService.IsOpenAIImagePoolRecoveryProbeEnabled(ctx)
	}
	return s.settingService.IsOpenAIPoolRecoveryProbeEnabled(ctx)
}

func (s *OpenAIGatewayService) openAIPoolRecoveryProbeUsesImagePool(account *Account, requestedModel string) bool {
	if account != nil && account.IsImagePoolMode() {
		return true
	}
	if s != nil && account != nil {
		cooldownContext := s.openAIPoolAccountSoftCooldownContext(account.ID)
		if cooldownContext.ProbeKind == "images" ||
			cooldownContext.ProbeCapability == OpenAIImagesCapabilityBasic ||
			cooldownContext.ProbeCapability == OpenAIImagesCapabilityNative ||
			isOpenAIImageGenerationModel(cooldownContext.ProbeModel) {
			return true
		}
	}
	return isOpenAIImageGenerationModel(requestedModel)
}

func (s *OpenAIGatewayService) openAIPoolRecoveryProbeTimeout(ctx context.Context, account *Account, requestedModel string) time.Duration {
	if s == nil || account == nil {
		return openAIPoolRecoveryProbeTimeout
	}
	if s.openAIPoolRecoveryProbeUsesImagePool(account, requestedModel) {
		if s.settingService != nil {
			if timeout := s.settingService.GetOpenAIImagePoolProbeTimeout(ctx); timeout > 0 {
				return timeout
			}
		}
		return openAIPoolRecoveryProbeImageTimeout
	}
	if s.settingService != nil {
		if timeout := s.settingService.GetOpenAIPoolProbeTimeout(ctx); timeout > 0 {
			return timeout
		}
	}
	return openAIPoolRecoveryProbeTimeout
}

func (s *OpenAIGatewayService) MaybeKickOpenAIPoolRecoveryProbeFromAdminList(ctx context.Context, account *Account) {
	if s == nil || account == nil || !account.IsOpenAI() || !account.IsPoolMode() {
		return
	}
	cooldownUntil, ok := s.openAIPoolAccountSoftCooldownUntil(account)
	if !ok || time.Now().Before(cooldownUntil) {
		return
	}
	if _, probing := s.openaiPoolRecoveryProbeInFlight.Load(account.ID); probing {
		return
	}
	now := time.Now()
	if value, ok := s.openaiPoolRecoveryProbeAdminKickAt.Load(account.ID); ok {
		if last, ok := value.(time.Time); ok && now.Sub(last) < openAIPoolRecoveryProbeAdminKickEvery {
			return
		}
	}
	s.openaiPoolRecoveryProbeAdminKickAt.Store(account.ID, now)
	cooldownContext := s.openAIPoolAccountSoftCooldownContext(account.ID)
	s.maybeStartOpenAIPoolRecoveryProbe(ctx, account, cooldownContext.ProbeModel)
}

func (s *OpenAIGatewayService) runOpenAIPoolRecoveryProbe(ctx context.Context, account *Account, requestedModel string, cooldownUntil time.Time) {
	result := s.probeOpenAIPoolAccountRecovery(ctx, account, requestedModel)
	if !s.openAIPoolAccountSoftCooldownMatches(account.ID, cooldownUntil) {
		loggerLegacyOpenAIPoolRecovery("probe_result_ignored_stale account_id=%d endpoint=%s status=%d", account.ID, result.endpoint, result.statusCode)
		return
	}
	if result.success {
		s.ClearAccountSchedulingBlock(account.ID)
		s.openaiPoolRecoveryProbeFailureCount.Delete(account.ID)
		if s.rateLimitService != nil {
			if _, err := s.rateLimitService.RecoverAccountAfterSuccessfulTest(ctx, account.ID); err != nil {
				loggerLegacyOpenAIPoolRecovery("recover_state_failed account_id=%d err=%v", account.ID, err)
			}
		}
		loggerLegacyOpenAIPoolRecovery("probe_success account_id=%d endpoint=%s status=%d", account.ID, result.endpoint, result.statusCode)
		return
	}

	backoff := s.nextOpenAIPoolRecoveryProbeBackoff(account.ID, result.retryable)
	until := time.Now().Add(backoff)
	s.storeOpenAIPoolSoftCooldownUntil(account.ID, until)
	s.storeOpenAIPoolRecoveryProbeBackoffContext(account.ID, result)
	if result.err != nil {
		loggerLegacyOpenAIPoolRecovery("probe_failed account_id=%d endpoint=%s status=%d backoff=%s err=%v", account.ID, result.endpoint, result.statusCode, backoff, result.err)
	} else {
		loggerLegacyOpenAIPoolRecovery("probe_failed account_id=%d endpoint=%s status=%d backoff=%s", account.ID, result.endpoint, result.statusCode, backoff)
	}
}

func (s *OpenAIGatewayService) probeOpenAIPoolAccountRecovery(ctx context.Context, account *Account, requestedModel string) openAIPoolRecoveryProbeResult {
	cooldownContext := s.openAIPoolAccountSoftCooldownContext(account.ID)
	switch {
	case cooldownContext.ProbeKind == "images":
		return s.probeOpenAIPoolAccountImages(ctx, account, cooldownContext.ProbeCapability, s.resolveOpenAIImagePoolRecoveryProbeModel(ctx, account, cooldownContext.ProbeModel))
	case cooldownContext.ProbeCapability == OpenAIImagesCapabilityBasic || cooldownContext.ProbeCapability == OpenAIImagesCapabilityNative:
		return s.probeOpenAIPoolAccountImages(ctx, account, cooldownContext.ProbeCapability, s.resolveOpenAIImagePoolRecoveryProbeModel(ctx, account, cooldownContext.ProbeModel))
	}
	model := s.resolveOpenAIPoolRecoveryProbeModel(ctx, account, requestedModel, cooldownContext)
	if account.Type == AccountTypeAPIKey {
		if openai_compat.ShouldUseResponsesAPI(account.Extra) {
			result := s.probeOpenAIPoolAccountResponses(ctx, account, model)
			if result.success || (result.statusCode != http.StatusNotFound && result.statusCode != http.StatusMethodNotAllowed) {
				return result
			}
		}
		return s.probeOpenAIPoolAccountChatCompletions(ctx, account, model)
	}
	return s.probeOpenAIPoolAccountResponses(ctx, account, model)
}

func (s *OpenAIGatewayService) resolveOpenAIPoolRecoveryProbeModel(ctx context.Context, account *Account, requestedModel string, cooldownContext openAIPoolSoftCooldownContext) string {
	for _, value := range []string{s.configuredOpenAIPoolRecoveryProbeModel(ctx), openai.DefaultTestModel} {
		if model := strings.TrimSpace(value); model != "" {
			if account != nil {
				if mapped := account.GetMappedModel(model); strings.TrimSpace(mapped) != "" {
					return mapped
				}
			}
			return model
		}
	}
	return openai.DefaultTestModel
}

func (s *OpenAIGatewayService) configuredOpenAIPoolRecoveryProbeModel(ctx context.Context) string {
	if s != nil && s.settingService != nil {
		return strings.TrimSpace(s.settingService.GetOpenAIPoolRecoveryProbeModel(ctx))
	}
	return openai.DefaultTestModel
}

func (s *OpenAIGatewayService) configuredOpenAIImagePoolRecoveryProbeModel(ctx context.Context) string {
	if s != nil && s.settingService != nil {
		return strings.TrimSpace(s.settingService.GetOpenAIImagePoolRecoveryProbeModel(ctx))
	}
	return openAIPoolRecoveryProbeDefaultImageModel
}

func (s *OpenAIGatewayService) resolveOpenAIImagePoolRecoveryProbeModel(ctx context.Context, account *Account, requestedModel string) string {
	for _, value := range []string{requestedModel, s.configuredOpenAIImagePoolRecoveryProbeModel(ctx), openAIPoolRecoveryProbeDefaultImageModel} {
		if model := strings.TrimSpace(value); model != "" {
			if account != nil {
				if mapped := account.GetMappedModel(model); strings.TrimSpace(mapped) != "" {
					return mapped
				}
			}
			return model
		}
	}
	return openAIPoolRecoveryProbeDefaultImageModel
}

func openAIPoolCooldownShouldAvoidOriginalProbeModel(cooldownContext openAIPoolSoftCooldownContext) bool {
	return isOpenAIPoolDownstreamRoutingOrClientConfigError(
		cooldownContext.StatusCode,
		cooldownContext.Reason,
		[]byte(cooldownContext.LastProbeReason),
	)
}

func (s *OpenAIGatewayService) openAIPoolRecoveryProbeFallbackModel(ctx context.Context) string {
	if s == nil || s.settingService == nil {
		return ""
	}
	return strings.TrimSpace(s.settingService.GetFallbackModel(ctx, PlatformOpenAI))
}

func (s *OpenAIGatewayService) storeOpenAIPoolRecoveryProbeBackoffContext(accountID int64, result openAIPoolRecoveryProbeResult) {
	if s == nil || accountID <= 0 {
		return
	}
	cooldownContext := s.openAIPoolAccountSoftCooldownContext(accountID)
	cooldownContext.CooldownSource = "probe_backoff"
	cooldownContext.LastProbeStatus = result.statusCode
	if result.err != nil {
		cooldownContext.LastProbeReason = result.err.Error()
	} else if result.statusCode > 0 {
		cooldownContext.LastProbeReason = http.StatusText(result.statusCode)
	}
	cooldownContext.LastProbeReason = truncateString(strings.TrimSpace(cooldownContext.LastProbeReason), 256)
	s.openaiPoolSoftCooldownContext.Store(accountID, cooldownContext)
}

func (s *OpenAIGatewayService) probeOpenAIPoolAccountImages(ctx context.Context, account *Account, capability OpenAIImagesCapability, requestedModel string) openAIPoolRecoveryProbeResult {
	if capability == "" {
		capability = OpenAIImagesCapabilityBasic
	}
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		model = openAIPoolRecoveryProbeDefaultImageModel
	}
	model = account.GetMappedModel(model)
	if strings.TrimSpace(model) == "" {
		model = openAIPoolRecoveryProbeDefaultImageModel
	}
	if account.Type != AccountTypeAPIKey {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "images", err: fmt.Errorf("image recovery probe unsupported for account type %s", account.Type)}
	}
	token, err := s.openAIRecoveryProbeToken(ctx, account)
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "images", err: err}
	}
	baseURL := account.GetOpenAIBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "images", err: err}
	}
	body := openAIRecoveryImagesPayload(model, capability)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, buildOpenAIImagesURL(validatedURL, openAIImagesGenerationsEndpoint), bytes.NewReader(body))
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "images", err: err}
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return s.doOpenAIPoolRecoveryProbe(req, account, "images")
}

func (s *OpenAIGatewayService) probeOpenAIPoolAccountImagesLegacy(ctx context.Context, account *Account, capability OpenAIImagesCapability) openAIPoolRecoveryProbeResult {
	switch capability {
	case OpenAIImagesCapabilityBasic, OpenAIImagesCapabilityNative:
		return s.probeOpenAIPoolAccountImages(ctx, account, capability, "")
	}
	return s.probeOpenAIPoolAccountImages(ctx, account, OpenAIImagesCapabilityBasic, "")
}

func (s *OpenAIGatewayService) probeOpenAIPoolAccountResponses(ctx context.Context, account *Account, model string) openAIPoolRecoveryProbeResult {
	token, err := s.openAIRecoveryProbeToken(ctx, account)
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "responses", err: err}
	}
	targetURL, err := s.openAIRecoveryProbeResponsesURL(account)
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "responses", err: err}
	}
	body := openAIRecoveryResponsesPayload(model, account.IsOAuth())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "responses", err: err}
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if account.IsOAuth() {
		req.Host = "chatgpt.com"
		if chatgptAccountID := account.GetChatGPTAccountID(); chatgptAccountID != "" {
			req.Header.Set("chatgpt-account-id", chatgptAccountID)
		}
	}
	return s.doOpenAIPoolRecoveryProbe(req, account, "responses")
}

func (s *OpenAIGatewayService) probeOpenAIPoolAccountChatCompletions(ctx context.Context, account *Account, model string) openAIPoolRecoveryProbeResult {
	token, err := s.openAIRecoveryProbeToken(ctx, account)
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "chat_completions", err: err}
	}
	baseURL := account.GetOpenAIBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "chat_completions", err: err}
	}
	body := openAIRecoveryChatCompletionsPayload(model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, buildOpenAIChatCompletionsURL(validatedURL), bytes.NewReader(body))
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: "chat_completions", err: err}
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return s.doOpenAIPoolRecoveryProbe(req, account, "chat_completions")
}

func (s *OpenAIGatewayService) doOpenAIPoolRecoveryProbe(req *http.Request, account *Account, endpoint string) openAIPoolRecoveryProbeResult {
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.resolveTLSProfile(account))
	if err != nil {
		return openAIPoolRecoveryProbeResult{retryable: true, endpoint: endpoint, err: err}
	}
	defer resp.Body.Close()
	_, readErr := io.ReadAll(io.LimitReader(resp.Body, openAIPoolRecoveryProbeReadLimit))
	if readErr != nil {
		return openAIPoolRecoveryProbeResult{
			retryable:      true,
			statusCode:     resp.StatusCode,
			endpoint:       endpoint,
			responseHeader: resp.Header,
			err:            readErr,
		}
	}
	return openAIPoolRecoveryProbeResult{
		success:        resp.StatusCode >= 200 && resp.StatusCode < 300,
		retryable:      openAIPoolRecoveryProbeStatusRetryable(resp.StatusCode),
		statusCode:     resp.StatusCode,
		endpoint:       endpoint,
		responseHeader: resp.Header,
	}
}

func (s *OpenAIGatewayService) openAIRecoveryProbeToken(ctx context.Context, account *Account) (string, error) {
	switch account.Type {
	case AccountTypeOAuth:
		if s.openAITokenProvider != nil {
			return s.openAITokenProvider.GetAccessToken(ctx, account)
		}
		token := strings.TrimSpace(account.GetOpenAIAccessToken())
		if token == "" {
			return "", fmt.Errorf("access_token not found in credentials")
		}
		return token, nil
	case AccountTypeAPIKey:
		token := strings.TrimSpace(account.GetOpenAIApiKey())
		if token == "" {
			return "", fmt.Errorf("api_key not found in credentials")
		}
		return token, nil
	default:
		return "", fmt.Errorf("unsupported account type: %s", account.Type)
	}
}

func (s *OpenAIGatewayService) openAIRecoveryProbeResponsesURL(account *Account) (string, error) {
	if account.IsOAuth() {
		return chatgptCodexURL, nil
	}
	baseURL := account.GetOpenAIBaseURL()
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return "", err
	}
	return buildOpenAIResponsesURL(validatedURL), nil
}

func openAIRecoveryResponsesPayload(model string, oauth bool) []byte {
	if strings.TrimSpace(model) == "" {
		model = openai.DefaultTestModel
	}
	body := map[string]any{
		"model": model,
		"input": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "input_text", "text": "hi"},
				},
			},
		},
		"instructions":      openai.DefaultInstructions,
		"stream":            false,
		"max_output_tokens": openAIPoolRecoveryProbeMaxOutputTokens,
	}
	if oauth {
		body["store"] = false
	}
	payload, _ := json.Marshal(body)
	return payload
}

func openAIRecoveryChatCompletionsPayload(model string) []byte {
	if strings.TrimSpace(model) == "" {
		model = openai.DefaultTestModel
	}
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		"stream":     false,
		"max_tokens": openAIPoolRecoveryProbeMaxOutputTokens,
	})
	return payload
}

func openAIRecoveryImagesPayload(model string, capability OpenAIImagesCapability) []byte {
	if strings.TrimSpace(model) == "" {
		model = openAIPoolRecoveryProbeDefaultImageModel
	}
	body := map[string]any{
		"model":  model,
		"prompt": "small test image",
		"n":      1,
	}
	if capability == OpenAIImagesCapabilityNative {
		body["size"] = "1024x1024"
	}
	payload, _ := json.Marshal(body)
	return payload
}

func openAIPoolRecoveryProbeStatusRetryable(statusCode int) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return false
	default:
		return true
	}
}

func (s *OpenAIGatewayService) nextOpenAIPoolRecoveryProbeBackoff(accountID int64, retryable bool) time.Duration {
	if !retryable {
		return openAIPoolSoftCooldownAuth
	}
	failures := 1
	if current, ok := s.openaiPoolRecoveryProbeFailureCount.Load(accountID); ok {
		if count, ok := current.(int); ok && count > 0 {
			failures = count + 1
		}
	}
	s.openaiPoolRecoveryProbeFailureCount.Store(accountID, failures)
	backoff := openAIPoolRecoveryProbeDefaultBackoff
	for i := 1; i < failures; i++ {
		backoff *= 2
		if backoff >= openAIPoolRecoveryProbeMaxBackoff {
			return openAIPoolRecoveryProbeMaxBackoff
		}
	}
	return backoff
}

func loggerLegacyOpenAIPoolRecovery(format string, args ...any) {
	logger.LegacyPrintf("service.openai_pool_recovery", format, args...)
}
