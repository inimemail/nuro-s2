package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
)

const nonOpenAIPoolProbeResponseLimit = 1 << 20

func (s *AccountTestService) runNonOpenAIPoolProbe(ctx context.Context, accountID int64, platform, kind, model string) NonOpenAIPoolProbeResult {
	result := NonOpenAIPoolProbeResult{Source: "recovery_probe"}
	if s == nil || s.accountRepo == nil || s.httpUpstream == nil {
		result.Reason = "recovery probe service is not configured"
		return result
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		result.Reason = "failed to load account"
		return result
	}
	if !strings.EqualFold(account.Platform, platform) {
		result.Success = true
		return result
	}
	if !account.IsActive() || !account.Schedulable {
		// Do not probe accounts that were disabled while their cooldown was active.
		result.Success = true
		result.Source = "account_unschedulable"
		return result
	}
	if !account.IsPoolMode() || !account.IsPoolSoftCooldownEnabled() {
		result.Success = true
		return result
	}
	model = strings.TrimSpace(model)
	if model == "" {
		result.Reason = "recovery probe model is empty"
		return result
	}
	if account.Platform != PlatformAntigravity {
		model = strings.TrimSpace(account.GetMappedModel(model))
		if model == "" {
			result.Reason = "mapped recovery probe model is empty"
			return result
		}
	}
	var resp *http.Response
	switch account.Platform {
	case PlatformGemini:
		resp, err = s.probeGeminiOnce(ctx, account, model)
	case PlatformAntigravity:
		resp, err = s.probeAntigravityOnce(ctx, account, model)
	case PlatformGrok:
		resp, err = s.probeGrokOnce(ctx, account, kind, model)
	case PlatformKimi, PlatformZhipu, PlatformDeepSeek:
		resp, err = s.probeCNProviderOnce(ctx, account, model)
	default:
		err = fmt.Errorf("unsupported recovery probe platform: %s", account.Platform)
	}
	if err != nil {
		result.Reason = sanitizeUpstreamErrorMessage(err.Error())
		return result
	}
	if resp == nil {
		result.Reason = "upstream returned no response"
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, nonOpenAIPoolProbeResponseLimit))
	result.StatusCode = resp.StatusCode
	if readErr != nil {
		result.Reason = sanitizeUpstreamErrorMessage(readErr.Error())
		return result
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Reason = extractUpstreamErrorMessage(body)
		if result.Reason == "" {
			result.Reason = http.StatusText(resp.StatusCode)
		}
		return result
	}
	if !nonOpenAIPoolProbeResponseValid(account, kind, model, body) {
		result.StatusCode = http.StatusBadGateway
		result.Reason = "upstream returned an invalid probe response"
		return result
	}
	result.Success = true
	return result
}

func (s *AccountTestService) probeGeminiOnce(ctx context.Context, account *Account, model string) (*http.Response, error) {
	payload := createGeminiTestPayload(model, ".")
	var req *http.Request
	var err error
	switch account.Type {
	case AccountTypeAPIKey:
		req, err = s.buildGeminiAPIKeyRequest(ctx, account, model, payload)
	case AccountTypeOAuth:
		req, err = s.buildGeminiOAuthRequest(ctx, account, model, payload)
	case AccountTypeServiceAccount:
		req, err = s.buildGeminiServiceAccountRequest(ctx, account, model, payload)
	default:
		err = fmt.Errorf("unsupported gemini account type: %s", account.Type)
	}
	if err != nil {
		return nil, err
	}
	account.ApplyHeaderOverrides(req.Header)
	return s.doNonOpenAIPoolProbe(req, account)
}

func (s *AccountTestService) probeAntigravityOnce(ctx context.Context, account *Account, model string) (*http.Response, error) {
	if account.Type == AccountTypeAPIKey {
		mappedModel := strings.TrimSpace(account.GetMappedModel(model))
		if mappedModel == "" {
			return nil, errors.New("mapped recovery probe model is empty")
		}
		if strings.HasPrefix(strings.ToLower(mappedModel), "gemini-") {
			return s.probeGeminiOnce(ctx, account, mappedModel)
		}
		return s.probeAnthropicOnce(ctx, account, mappedModel, account.GetBaseURL(), account.GetCredential("api_key"))
	}
	if s.antigravityGatewayService == nil {
		return nil, errors.New("antigravity gateway service is not configured")
	}
	return s.antigravityGatewayService.probeOnce(ctx, account, model)
}

func (s *AccountTestService) probeGrokOnce(ctx context.Context, account *Account, kind, model string) (*http.Response, error) {
	var token string
	var err error
	if account.Type == AccountTypeOAuth {
		if s.grokTokenProvider == nil {
			return nil, errors.New("grok token provider is not configured")
		}
		token, err = s.grokTokenProvider.GetAccessTokenForProbe(ctx, account)
	} else {
		token = strings.TrimSpace(account.GetGrokAccessToken())
		if token == "" {
			err = errors.New("grok API key is missing")
		}
	}
	if err != nil {
		return nil, err
	}
	var targetURL string
	payload := map[string]any{}
	if kind == NonOpenAIPoolRequestKindImage {
		endpoint := GrokMediaEndpointImagesGenerations
		if strings.Contains(strings.ToLower(model), "video") {
			endpoint = GrokMediaEndpointVideosGenerations
		}
		// Keep recovery probes on the same upstream model normalization path as
		// real media requests. In particular, xAI's 1.5 video alias is not the
		// model accepted by a text-to-video generation request.
		model = normalizeGrokMediaModelForEndpoint(endpoint, model, false)
		payload = map[string]any{"model": model, "prompt": "."}
		if endpoint == GrokMediaEndpointImagesGenerations {
			payload["n"] = 1
		}
		targetURL, err = buildGrokMediaURL(account, s.cfg, endpoint, "")
	} else {
		targetURL, err = buildGrokResponsesURL(account, s.cfg)
		payload = map[string]any{"model": model, "input": ".", "max_output_tokens": 1, "stream": false}
	}
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	applyGrokOAuthIdentityHeaders(req.Header, targetURL, account.IsGrokOAuth())
	account.ApplyHeaderOverrides(req.Header)
	return s.doNonOpenAIPoolProbe(req, account)
}

func (s *AccountTestService) probeCNProviderOnce(ctx context.Context, account *Account, model string) (*http.Response, error) {
	apiKey := strings.TrimSpace(account.GetCNAPIKey())
	if apiKey == "" {
		return nil, errors.New("API key is missing")
	}
	switch account.GetAPIProtocol() {
	case APIProtocolAnthropic, APIProtocolAdaptive:
		return s.probeAnthropicOnce(ctx, account, model, account.GetCNProtocolBaseURL(APIProtocolAnthropic), apiKey)
	case APIProtocolResponses:
		baseURL, err := s.validateUpstreamBaseURL(account.GetCNProtocolBaseURL(APIProtocolResponses))
		if err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(map[string]any{"model": model, "input": ".", "max_output_tokens": 1, "stream": false, "store": false})
		return s.probeOpenAICompatibleOnce(ctx, account, buildOpenAIResponsesURLForPlatform(account.Platform, baseURL), apiKey, payload)
	default:
		baseURL, err := s.validateUpstreamBaseURL(account.GetCNProtocolBaseURL(APIProtocolChatCompletions))
		if err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "."}}, "max_tokens": 1, "stream": false})
		return s.probeOpenAICompatibleOnce(ctx, account, buildOpenAIChatCompletionsURL(baseURL), apiKey, payload)
	}
}

func (s *AccountTestService) probeOpenAICompatibleOnce(ctx context.Context, account *Account, targetURL, apiKey string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	account.ApplyHeaderOverrides(req.Header)
	return s.doNonOpenAIPoolProbe(req, account)
}

func (s *AccountTestService) probeAnthropicOnce(ctx context.Context, account *Account, model, baseURL, apiKey string) (*http.Response, error) {
	baseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "."}}, "max_tokens": 1, "stream": false})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(baseURL, "/")+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	setAnthropicAPIKeyAuthHeader(req.Header, account, apiKey)
	account.ApplyHeaderOverrides(req.Header)
	return s.doNonOpenAIPoolProbe(req, account)
}

func (s *AccountTestService) doNonOpenAIPoolProbe(req *http.Request, account *Account) (*http.Response, error) {
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	if s.tlsFPProfileService == nil {
		return s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, nil)
	}
	return s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
}

func nonOpenAIPoolProbeResponseValid(account *Account, kind, model string, body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if account == nil || len(trimmed) == 0 {
		return false
	}
	if account.Platform == PlatformGrok && kind == NonOpenAIPoolRequestKindImage {
		endpoint := GrokMediaEndpointImagesGenerations
		if strings.Contains(strings.ToLower(model), "video") {
			endpoint = GrokMediaEndpointVideosGenerations
		}
		return grokMediaSuccessResponseIsValid(endpoint, trimmed)
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		validFrame := false
		for _, line := range bytes.Split(trimmed, []byte{'\n'}) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			if nonOpenAIPoolProbeJSONResponseValid(account, payload) {
				validFrame = true
				continue
			}
			// Gemini-compatible streams may finish with a metadata-only frame.
			// It is not content, but a prior valid content frame remains enough.
			if nonOpenAIPoolProbeSSEMetadataFrame(account, payload) {
				continue
			}
			return false
		}
		return validFrame
	}
	return nonOpenAIPoolProbeJSONResponseValid(account, trimmed)
}

func nonOpenAIPoolProbeSSEMetadataFrame(account *Account, payload []byte) bool {
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil || object["error"] != nil || account == nil {
		return false
	}
	if account.Platform != PlatformGemini && account.Platform != PlatformAntigravity {
		return false
	}
	// Antigravity wraps Gemini response fields below response.
	if response, ok := object["response"].(map[string]any); ok {
		if response["error"] != nil {
			return false
		}
		object = response
	}
	if status, ok := object["status"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "failed", "failure", "cancelled", "canceled", "expired", "incomplete":
			return false
		}
	}
	for _, key := range []string{"usageMetadata", "modelVersion", "responseId"} {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

func nonOpenAIPoolProbeJSONResponseValid(account *Account, trimmed []byte) bool {
	var object map[string]any
	if err := json.Unmarshal(trimmed, &object); err != nil || object["error"] != nil {
		return false
	}
	if blocked, ok := object["promptFeedback"].(map[string]any); ok {
		if reason, ok := blocked["blockReason"].(string); ok && strings.TrimSpace(reason) != "" && !strings.EqualFold(reason, "none") {
			return false
		}
	}
	if status, ok := object["status"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "failed", "failure", "cancelled", "canceled", "expired", "incomplete":
			return false
		}
	}
	hasArray := func(key string) bool {
		values, ok := object[key].([]any)
		return ok && len(values) > 0
	}
	hasArrayField := func(key string) bool {
		_, ok := object[key].([]any)
		return ok
	}
	hasString := func(key string) bool {
		value, ok := object[key].(string)
		return ok && strings.TrimSpace(value) != ""
	}
	hasUsableGeminiCandidate := func(values []any) bool {
		if len(values) == 0 {
			return false
		}
		for _, value := range values {
			candidate, ok := value.(map[string]any)
			if !ok {
				return false
			}
			if reason, ok := candidate["finishReason"].(string); ok && strings.TrimSpace(reason) != "" {
				switch strings.ToUpper(strings.TrimSpace(reason)) {
				case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "OTHER":
					return false
				}
				if geminiFinishReasonAllowsSuccessSideEffects(reason) {
					continue
				}
				return false
			}
			content, ok := candidate["content"].(map[string]any)
			if !ok {
				return false
			}
			parts, ok := content["parts"].([]any)
			if !ok || len(parts) == 0 {
				return false
			}
		}
		return true
	}
	hasNestedCandidates := func() bool {
		response, ok := object["response"].(map[string]any)
		if !ok || response["error"] != nil {
			return false
		}
		if blocked, ok := response["promptFeedback"].(map[string]any); ok {
			if reason, ok := blocked["blockReason"].(string); ok && strings.TrimSpace(reason) != "" && !strings.EqualFold(reason, "none") {
				return false
			}
		}
		candidates, ok := response["candidates"].([]any)
		return ok && hasUsableGeminiCandidate(candidates)
	}
	switch account.Platform {
	case PlatformGemini:
		if candidates, ok := object["candidates"].([]any); ok {
			return hasUsableGeminiCandidate(candidates)
		}
		return hasNestedCandidates()
	case PlatformAntigravity:
		if candidates, ok := object["candidates"].([]any); ok {
			return hasUsableGeminiCandidate(candidates)
		}
		return hasNestedCandidates() || hasArray("content") || hasString("id")
	case PlatformGrok:
		return hasArrayField("output")
	case PlatformKimi, PlatformZhipu, PlatformDeepSeek:
		switch account.GetAPIProtocol() {
		case APIProtocolAnthropic, APIProtocolAdaptive:
			return hasArray("content")
		case APIProtocolResponses:
			return hasArrayField("output")
		default:
			return hasArray("choices")
		}
	default:
		return false
	}
}

func (s *AntigravityGatewayService) probeOnce(ctx context.Context, account *Account, model string) (*http.Response, error) {
	if s == nil || s.tokenProvider == nil || s.httpUpstream == nil {
		return nil, errors.New("antigravity probe service is not configured")
	}
	accessToken, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	mappedModel := s.getMappedModel(account, model)
	if mappedModel == "" {
		return nil, fmt.Errorf("model %s is not available for this account", model)
	}
	projectID := strings.TrimSpace(account.GetCredential("project_id"))
	var body []byte
	if strings.HasPrefix(strings.ToLower(mappedModel), "gemini-") {
		body, err = s.buildGeminiTestRequest(projectID, mappedModel)
	} else {
		body, err = s.buildClaudeTestRequest(projectID, mappedModel)
	}
	if err != nil {
		return nil, err
	}
	baseURL := resolveAntigravityForwardBaseURL()
	if baseURL == "" {
		return nil, errors.New("no antigravity forward base URL configured")
	}
	req, err := antigravity.NewAPIRequestWithURL(ctx, baseURL, "streamGenerateContent", accessToken, body)
	if err != nil {
		return nil, err
	}
	account.ApplyHeaderOverrides(req.Header)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	return s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.resolveTLSProfile(account))
}
