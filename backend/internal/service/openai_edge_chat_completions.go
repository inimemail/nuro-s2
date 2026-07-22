package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type OpenAIEdgePreparedChatCompletions struct {
	Plan            OpenAIEdgePlan
	Model           string
	BillingModel    string
	UpstreamModel   string
	ReasoningEffort *string
	ServiceTier     *string
}

// BuildRawChatCompletionsEdgePlan builds an executable edge plan only for the
// raw OpenAI-compatible Chat Completions path. The Responses-conversion path
// still belongs to Go because it must translate upstream SSE frames.
func (s *OpenAIGatewayService) BuildRawChatCompletionsEdgePlan(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIEdgePreparedChatCompletions, error) {
	if s == nil {
		return nil, fmt.Errorf("openai gateway service is nil")
	}
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	if err := s.checkOpenAIEdgeLocalAccountPolicy(ctx, c, account, body); err != nil {
		return nil, err
	}
	if account.Type != AccountTypeAPIKey || shouldForwardAPIKeyChatViaResponses(account) {
		return nil, fmt.Errorf("account requires Go chat/responses conversion")
	}
	if !gjson.GetBytes(body, "stream").Bool() {
		return nil, fmt.Errorf("edge raw chat relay requires stream=true")
	}

	originalModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if originalModel == "" {
		return nil, fmt.Errorf("missing model in request")
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, originalModel)
	serviceTier := extractOpenAIServiceTierFromBody(body)
	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)

	upstreamBody := body
	if upstreamModel != originalModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}
	updatedBody, policyErr := s.applyOpenAIFastPolicyToBody(ctx, account, upstreamModel, upstreamBody)
	if policyErr != nil {
		return nil, policyErr
	}
	upstreamBody = updatedBody
	var err error
	upstreamBody, err = ensureOpenAIChatStreamUsage(upstreamBody)
	if err != nil {
		return nil, fmt.Errorf("enable stream usage: %w", err)
	}
	upstreamBody, cacheCreationOptimization, err := s.ApplyOpenAIPromptCacheCreationOptimizationBody(account, upstreamModel, upstreamBody)
	if err != nil {
		return nil, err
	}
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		isolatedBody, isolated, isolationErr := applyOpenAIUpstreamStrongIsolationBody(upstreamBody, false)
		if isolationErr != nil {
			return nil, fmt.Errorf("apply upstream strong isolation: %w", isolationErr)
		}
		if isolated {
			upstreamBody = isolatedBody
		}
	}
	cacheCreationMode, cacheCreationModel := openAIEdgePromptCacheCreationOptimizationFields(account, upstreamModel, cacheCreationOptimization)

	apiKey := account.GetOpenAIApiKey()
	if apiKey == "" {
		return nil, fmt.Errorf("account %d missing api_key", account.ID)
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url: %w", err)
	}

	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + apiKey,
		"Accept":        "text/event-stream",
	}
	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			if openaiCCRawAllowedHeaders[strings.ToLower(key)] && len(values) > 0 {
				headers[key] = values[0]
			}
		}
	}
	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		headers["user-agent"] = customUA
	}
	applyOpenAIEdgeHeaderOverrides(account, headers)
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		scrubOpenAIEdgeStrongIsolationHeaders(headers)
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	return &OpenAIEdgePreparedChatCompletions{
		Plan: OpenAIEdgePlan{
			Action:          OpenAIEdgeActionRelay,
			AccountID:       account.ID,
			AccountType:     account.Type,
			Transport:       OpenAIEdgeTransportHTTP2SSE,
			ResponseDialect: OpenAIEdgeDialectChatCompletions,
			UpstreamURL:     buildOpenAIChatCompletionsURL(validatedURL),
			Headers:         headers,
			BodyRawBase64:   EncodeOpenAIEdgeRawBody(upstreamBody),
			ProxyURL:        proxyURL,
			FirstTokenTimeoutPlaceholderMS: s.openAIStreamFirstTokenTimeoutPlaceholderMs(
				account,
				originalModel,
			),
			PromptCacheCreationOptimizationMode:    cacheCreationMode,
			PromptCacheCreationOptimizationModel:   cacheCreationModel,
			PromptCacheCreationOptimizationApplied: cacheCreationOptimization.Applied,
		},
		Model:           originalModel,
		BillingModel:    billingModel,
		UpstreamModel:   upstreamModel,
		ReasoningEffort: reasoningEffort,
		ServiceTier:     serviceTier,
	}, nil
}

func IsOpenAIEdgeRawChatRelayEligible(account *Account) bool {
	return account != nil &&
		account.Type == AccountTypeAPIKey &&
		!shouldForwardAPIKeyChatViaResponses(account)
}

func OpenAIEdgeRawChatUpstreamEndpoint(account *Account) string {
	if IsOpenAIEdgeRawChatRelayEligible(account) {
		return "/v1/chat/completions"
	}
	return ""
}

func IsOpenAIEdgeRawRelayEligibleForInboundEndpoint(account *Account, inboundEndpoint string) bool {
	switch strings.TrimSpace(inboundEndpoint) {
	case "/v1/chat/completions":
		return IsOpenAIEdgeRawChatRelayEligible(account)
	case "/v1/responses":
		return IsOpenAIEdgeRawResponsesRelayEligible(account)
	default:
		return false
	}
}

func OpenAIEdgeRawUpstreamEndpointForInbound(account *Account, inboundEndpoint string) string {
	switch strings.TrimSpace(inboundEndpoint) {
	case "/v1/chat/completions":
		return OpenAIEdgeRawChatUpstreamEndpoint(account)
	case "/v1/responses":
		if IsOpenAIEdgeRawResponsesRelayEligible(account) {
			return "/v1/responses"
		}
	}
	return ""
}

func OpenAIEdgeHTTPStatusRetryable(status int) bool {
	return status == http.StatusUnauthorized ||
		status == http.StatusForbidden ||
		status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status == http.StatusInternalServerError ||
		status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout ||
		status == 529
}

// BuildRawResponsesEdgePlan builds an executable edge plan for the narrow
// native OpenAI Responses passthrough stream path. All account selection,
// billing, cooling, priority, sticky routing, and slot ownership still happen
// in Go before this is called.
func (s *OpenAIGatewayService) BuildRawResponsesEdgePlan(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*OpenAIEdgePreparedChatCompletions, error) {
	if s == nil {
		return nil, fmt.Errorf("openai gateway service is nil")
	}
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	if err := s.checkOpenAIEdgeLocalAccountPolicy(ctx, c, account, body); err != nil {
		return nil, err
	}
	if account.Type != AccountTypeAPIKey || !account.IsOpenAIPassthroughEnabled() || !openai_compat.ShouldUseResponsesAPI(account.Extra) {
		return nil, fmt.Errorf("account requires Go responses conversion")
	}
	if !gjson.GetBytes(body, "stream").Bool() {
		return nil, fmt.Errorf("edge raw responses relay requires stream=true")
	}
	if suffix := strings.TrimSpace(openAIResponsesRequestPathSuffix(c)); suffix != "" {
		return nil, fmt.Errorf("responses path suffix requires Go")
	}
	originalModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if originalModel == "" {
		return nil, fmt.Errorf("missing model in request")
	}
	explicitImageIntent := IsExplicitImageGenerationIntent(openAIResponsesEndpoint, originalModel, body)
	if explicitImageIntent {
		return nil, fmt.Errorf("image responses require Go")
	}
	if previousResponseID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()); previousResponseID != "" {
		return nil, fmt.Errorf("previous_response_id requires Go WSv2 state")
	}

	reasoningEffort := extractOpenAIReasoningEffortFromBody(body, originalModel)
	serviceTier := extractOpenAIServiceTierFromBody(body)
	upstreamBody, sanitized, err := sanitizeEmptyBase64InputImagesInOpenAIBody(body)
	if err != nil {
		return nil, err
	}
	if !sanitized {
		upstreamBody = body
	}
	if account.IsOpenAIResponsesPassthroughCompatEnabled() {
		upstreamBody, _, err = normalizeOpenAIResponsesStringInputBody(upstreamBody)
		if err != nil {
			return nil, err
		}
		upstreamBody, _, err = normalizeOpenAIAPIKeyResponsesUnsupportedParamsBody(upstreamBody)
		if err != nil {
			return nil, err
		}
	}
	if account.IsOpenAIResponsesArgumentsObjectCompatEnabled() {
		upstreamBody, _, err = normalizeOpenAIResponsesInputArgumentsBody(upstreamBody)
		if err != nil {
			return nil, err
		}
	}
	policyModel := strings.TrimSpace(gjson.GetBytes(upstreamBody, "model").String())
	if policyModel == "" {
		policyModel = originalModel
	}
	upstreamBody, err = s.applyOpenAIFastPolicyToBody(ctx, account, policyModel, upstreamBody)
	if err != nil {
		return nil, err
	}
	upstreamBody, cacheCreationOptimization, err := s.ApplyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account, policyModel, upstreamBody, explicitImageIntent)
	if err != nil {
		return nil, err
	}
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		isolatedBody, isolated, isolationErr := applyOpenAIUpstreamStrongIsolationBody(upstreamBody, true)
		if isolationErr != nil {
			return nil, fmt.Errorf("apply upstream strong isolation: %w", isolationErr)
		}
		if isolated {
			upstreamBody = isolatedBody
		}
	}
	cacheCreationMode, cacheCreationModel := openAIEdgePromptCacheCreationOptimizationFields(account, policyModel, cacheCreationOptimization)

	apiKey := account.GetOpenAIApiKey()
	if apiKey == "" {
		return nil, fmt.Errorf("account %d missing api_key", account.ID)
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url: %w", err)
	}

	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + apiKey,
		"Accept":        "text/event-stream",
	}
	allowTimeoutHeaders := s.isOpenAIPassthroughTimeoutHeadersAllowed()
	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lower := strings.ToLower(strings.TrimSpace(key))
			if !isOpenAIPassthroughAllowedRequestHeader(lower, allowTimeoutHeaders) || len(values) == 0 {
				continue
			}
			headers[key] = values[0]
		}
	}
	delete(headers, "authorization")
	delete(headers, "Authorization")
	delete(headers, "x-api-key")
	delete(headers, "X-Api-Key")
	delete(headers, "x-goog-api-key")
	delete(headers, "X-Goog-Api-Key")
	headers["Authorization"] = "Bearer " + apiKey
	if headers["Accept"] == "" {
		headers["Accept"] = "text/event-stream"
	}
	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		headers["user-agent"] = customUA
	}
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		headers["user-agent"] = codexCLIUserAgent
	}
	applyOpenAIEdgeHeaderOverrides(account, headers)
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		scrubOpenAIEdgeStrongIsolationHeaders(headers)
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	return &OpenAIEdgePreparedChatCompletions{
		Plan: OpenAIEdgePlan{
			Action:          OpenAIEdgeActionRelay,
			AccountID:       account.ID,
			AccountType:     account.Type,
			Transport:       OpenAIEdgeTransportHTTP2SSE,
			ResponseDialect: OpenAIEdgeDialectResponses,
			UpstreamURL:     buildOpenAIResponsesURL(validatedURL),
			Headers:         headers,
			BodyRawBase64:   EncodeOpenAIEdgeRawBody(upstreamBody),
			ProxyURL:        proxyURL,
			SafeTokenPlaceholder: s.openAIStreamSafeTokenPlaceholderEnabled(
				account,
				originalModel,
			),
			FirstTokenTimeoutPlaceholderMS: s.openAIStreamFirstTokenTimeoutPlaceholderMs(
				account,
				originalModel,
			),
			PromptCacheCreationOptimizationMode:    cacheCreationMode,
			PromptCacheCreationOptimizationModel:   cacheCreationModel,
			PromptCacheCreationOptimizationApplied: cacheCreationOptimization.Applied,
		},
		Model:           originalModel,
		BillingModel:    originalModel,
		UpstreamModel:   policyModel,
		ReasoningEffort: reasoningEffort,
		ServiceTier:     serviceTier,
	}, nil
}

// BuildChatGPTOAuthResponsesEdgePlan builds the OAuth/ChatGPT native Responses
// streaming edge plan. Go still owns auth, scheduling, billing preflight, and
// the Codex request transform; Rust only relays the already-prepared SSE stream.
func (s *OpenAIGatewayService) BuildChatGPTOAuthResponsesEdgePlan(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) (*OpenAIEdgePreparedChatCompletions, error) {
	if s == nil {
		return nil, fmt.Errorf("openai gateway service is nil")
	}
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	if err := s.checkOpenAIEdgeLocalAccountPolicy(ctx, c, account, body); err != nil {
		return nil, err
	}
	if account.Type != AccountTypeOAuth {
		return nil, fmt.Errorf("account is not oauth")
	}
	if !gjson.GetBytes(body, "stream").Bool() {
		return nil, fmt.Errorf("edge oauth responses relay requires stream=true")
	}
	if suffix := strings.TrimSpace(openAIResponsesRequestPathSuffix(c)); suffix != "" {
		return nil, fmt.Errorf("responses path suffix requires Go")
	}
	originalModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if originalModel == "" {
		return nil, fmt.Errorf("missing model in request")
	}
	explicitImageIntent := IsExplicitImageGenerationIntent(openAIResponsesEndpoint, originalModel, body)
	if explicitImageIntent {
		return nil, fmt.Errorf("image responses require Go")
	}
	if previousResponseID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()); previousResponseID != "" {
		return nil, fmt.Errorf("previous_response_id requires Go WSv2 state")
	}

	reqBody, err := getOpenAIRequestBodyMap(c, body)
	if err != nil {
		return nil, err
	}
	reqModel := originalModel
	if model, ok := reqBody["model"].(string); ok && strings.TrimSpace(model) != "" {
		reqModel = strings.TrimSpace(model)
	}
	promptCacheKey := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	if promptCacheKey == "" {
		if v, ok := reqBody["prompt_cache_key"].(string); ok {
			promptCacheKey = strings.TrimSpace(v)
		}
	}
	isCodexCLI := false
	if c != nil {
		isCodexCLI = openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator"))
	}
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		isCodexCLI = true
	}

	bodyModified := false
	if isInstructionsEmpty(reqBody) && !isOpenAICompatMessagesBridgeBody(body) {
		reqBody["instructions"] = "You are a helpful coding assistant."
		bodyModified = true
	}
	billingModel := account.GetMappedModel(reqModel)
	if billingModel != reqModel {
		reqBody["model"] = billingModel
		bodyModified = true
	}
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	if upstreamModel != "" && upstreamModel != billingModel {
		reqBody["model"] = upstreamModel
		bodyModified = true
	} else if upstreamModel == "" {
		upstreamModel = billingModel
	}
	if !SupportsVerbosity(upstreamModel) {
		if text, ok := reqBody["text"].(map[string]any); ok {
			if _, has := text["verbosity"]; has {
				delete(text, "verbosity")
				bodyModified = true
			}
		}
	}
	if reasoning, ok := reqBody["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort == "minimal" {
			reasoning["effort"] = "none"
			bodyModified = true
		}
	}

	codexResult := applyCodexOAuthTransform(reqBody, isCodexCLI, false)
	if codexResult.Modified {
		bodyModified = true
	}
	if applyCodexClientMetadata(reqBody, account) {
		bodyModified = true
	}
	if codexResult.NormalizedModel != "" {
		upstreamModel = codexResult.NormalizedModel
	}
	if codexResult.PromptCacheKey != "" {
		promptCacheKey = codexResult.PromptCacheKey
	}
	if account.IsOpenAIResponsesArgumentsObjectCompatEnabled() {
		if normalizeOpenAIResponsesInputArgumentsMap(reqBody) {
			bodyModified = true
		}
	}

	if s.isOpenAIPromptCacheBoostRuntimeEnabled(account) {
		if promptCacheKey == "" && s.isOpenAIPromptCacheBoostKeyRuntimeEnabled(account) {
			if generatedKey := deriveOpenAIVirtualPromptCacheKey(account, upstreamModel, body); generatedKey != "" {
				promptCacheKey = generatedKey
				if existing, ok := reqBody["prompt_cache_key"].(string); !ok || strings.TrimSpace(existing) == "" {
					reqBody["prompt_cache_key"] = generatedKey
					bodyModified = true
				}
			}
		}
		if s.isOpenAIPromptCacheBoostRetentionRuntimeEnabled(account) {
			if existing, ok := reqBody["prompt_cache_retention"].(string); !ok || strings.TrimSpace(existing) == "" {
				reqBody["prompt_cache_retention"] = "24h"
				bodyModified = true
			}
		}
	}
	if _, has := reqBody["safety_identifier"]; has {
		delete(reqBody, "safety_identifier")
		bodyModified = true
	}
	if !account.IsOpenAIPromptCacheBoostEnabled() {
		if _, has := reqBody["prompt_cache_retention"]; has {
			delete(reqBody, "prompt_cache_retention")
			bodyModified = true
		}
	}
	if sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody) {
		bodyModified = true
	}
	upstreamBody := body
	if bodyModified {
		upstreamBody, err = json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("serialize oauth edge request body: %w", err)
		}
	}
	upstreamBody, err = s.applyOpenAIFastPolicyToBody(ctx, account, upstreamModel, upstreamBody)
	if err != nil {
		return nil, err
	}
	upstreamBody, cacheCreationOptimization, err := s.ApplyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account, upstreamModel, upstreamBody, explicitImageIntent)
	if err != nil {
		return nil, err
	}
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		isolatedBody, isolated, isolationErr := applyOpenAIUpstreamStrongIsolationBody(upstreamBody, true)
		if isolationErr != nil {
			return nil, fmt.Errorf("apply upstream strong isolation: %w", isolationErr)
		}
		if isolated {
			upstreamBody = isolatedBody
		}
	}
	cacheCreationMode, cacheCreationModel := openAIEdgePromptCacheCreationOptimizationFields(account, upstreamModel, cacheCreationOptimization)

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	headers, headerErr := s.buildChatGPTOAuthEdgeHeaders(ctx, c, account, upstreamBody, token, promptCacheKey, isCodexCLI)
	if headerErr != nil {
		return nil, fmt.Errorf("build chatgpt oauth edge headers: %w", headerErr)
	}

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(upstreamBody, originalModel)
	serviceTier := extractOpenAIServiceTierFromBody(upstreamBody)
	return &OpenAIEdgePreparedChatCompletions{
		Plan: OpenAIEdgePlan{
			Action:          OpenAIEdgeActionRelay,
			AccountID:       account.ID,
			AccountType:     account.Type,
			Transport:       OpenAIEdgeTransportHTTP2SSE,
			ResponseDialect: OpenAIEdgeDialectResponses,
			UpstreamURL:     chatgptCodexURL,
			Headers:         headers,
			BodyRawBase64:   EncodeOpenAIEdgeRawBody(upstreamBody),
			ProxyURL:        proxyURL,
			SafeTokenPlaceholder: s.openAIStreamSafeTokenPlaceholderEnabled(
				account,
				originalModel,
			),
			FirstTokenTimeoutPlaceholderMS: s.openAIStreamFirstTokenTimeoutPlaceholderMs(
				account,
				originalModel,
			),
			PromptCacheCreationOptimizationMode:    cacheCreationMode,
			PromptCacheCreationOptimizationModel:   cacheCreationModel,
			PromptCacheCreationOptimizationApplied: cacheCreationOptimization.Applied,
		},
		Model:           originalModel,
		BillingModel:    billingModel,
		UpstreamModel:   upstreamModel,
		ReasoningEffort: reasoningEffort,
		ServiceTier:     serviceTier,
	}, nil
}

func (s *OpenAIGatewayService) buildChatGPTOAuthEdgeHeaders(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	promptCacheKey string,
	isCodexCLI bool,
) (map[string]string, error) {
	headers := http.Header{}
	authHeaders, authErr := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if authErr != nil {
		return nil, authErr
	}
	for key, values := range authHeaders {
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	if chatgptAccountID := account.GetChatGPTAccountID(); chatgptAccountID != "" {
		headers.Set("chatgpt-account-id", chatgptAccountID)
	}

	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			if !openaiAllowedHeaders[strings.ToLower(key)] {
				continue
			}
			for _, v := range values {
				headers.Add(key, v)
			}
		}
	}

	compatMessagesBridge := isOpenAICompatMessagesBridgeContext(c) || isOpenAICompatMessagesBridgeBody(body)
	clientConversationID := strings.TrimSpace(headers.Get("conversation_id"))
	headers.Del("conversation_id")
	headers.Del("session_id")
	if compatMessagesBridge {
		headers.Del("OpenAI-Beta")
		headers.Del("originator")
	} else {
		headers.Set("OpenAI-Beta", "responses=experimental")
		headers.Set("originator", resolveOpenAIUpstreamOriginator(c, isCodexCLI))
	}

	apiKeyID := getAPIKeyIDFromContext(c)
	if isOpenAIResponsesCompactPath(c) {
		headers.Set("accept", "application/json")
		if headers.Get("version") == "" {
			headers.Set("version", codexCLIVersion)
		}
		compactSession := resolveOpenAICompactSessionID(c)
		headers.Set("session_id", isolateOpenAISessionID(apiKeyID, compactSession))
	} else {
		headers.Set("accept", "text/event-stream")
	}
	if promptCacheKey != "" {
		isolated := isolateOpenAISessionID(apiKeyID, promptCacheKey)
		headers.Set("session_id", isolated)
		if !compatMessagesBridge || clientConversationID != "" {
			headers.Set("conversation_id", isolated)
		}
	}

	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		headers.Set("user-agent", customUA)
	}
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		headers.Set("user-agent", codexCLIUserAgent)
	}
	s.overrideBrowserUserAgentHeader(ctx, account, headers)
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		applyOpenAIUpstreamStrongIsolationHeaderMap(headers)
	}
	if headers.Get("content-type") == "" {
		headers.Set("content-type", "application/json")
	}

	result := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) > 0 {
			result[key] = values[0]
		}
	}
	return result, nil
}

func IsOpenAIEdgeRawResponsesRelayEligible(account *Account) bool {
	return account != nil &&
		account.Type == AccountTypeAPIKey &&
		account.IsOpenAIPassthroughEnabled() &&
		openai_compat.ShouldUseResponsesAPI(account.Extra)
}

func scrubOpenAIEdgeStrongIsolationHeaders(headers map[string]string) {
	if len(headers) == 0 {
		return
	}
	for key := range headers {
		lower := strings.ToLower(strings.TrimSpace(key))
		for _, header := range openAIUpstreamStrongIsolationHeaders {
			if lower == header {
				delete(headers, key)
				break
			}
		}
	}
}

func applyOpenAIEdgeHeaderOverrides(account *Account, headers map[string]string) {
	if account == nil || len(headers) == 0 {
		return
	}
	if len(account.GetHeaderOverrides()) == 0 {
		return
	}
	httpHeaders := http.Header{}
	for key, value := range headers {
		httpHeaders[key] = []string{value}
	}
	account.ApplyHeaderOverrides(httpHeaders)
	for key := range headers {
		delete(headers, key)
	}
	for key, values := range httpHeaders {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
}

// BuildResponsesWSEdgePlan builds an executable WSv2 passthrough plan for the
// Rust edge. It is intentionally limited to passthrough WSv2 mode because the
// ctx_pool/shared/dedicated modes own stateful previous_response_id handling in
// Go today.
func (s *OpenAIGatewayService) BuildResponsesWSEdgePlan(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	firstMessage []byte,
	token string,
) (*OpenAIEdgePreparedChatCompletions, error) {
	if s == nil {
		return nil, fmt.Errorf("openai gateway service is nil")
	}
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	if err := s.checkOpenAIEdgeLocalAccountPolicy(ctx, c, account, firstMessage); err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("token is empty")
	}
	if s.cfg == nil || !s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled {
		return nil, fmt.Errorf("edge ws requires mode_router_v2")
	}
	if account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault) != OpenAIWSIngressModePassthrough {
		return nil, fmt.Errorf("edge ws requires passthrough mode")
	}
	if account.Proxy != nil || account.ProxyID != nil {
		return nil, fmt.Errorf("edge ws proxy is not supported yet")
	}
	wsDecision := s.getOpenAIWSProtocolResolver().Resolve(account)
	if wsDecision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		return nil, fmt.Errorf("edge ws requires ws_v2 transport")
	}
	model := strings.TrimSpace(gjson.GetBytes(firstMessage, "model").String())
	if model == "" {
		return nil, fmt.Errorf("missing model in first ws message")
	}
	if previousResponseID := strings.TrimSpace(gjson.GetBytes(firstMessage, "previous_response_id").String()); previousResponseID != "" {
		return nil, fmt.Errorf("previous_response_id requires Go WS state")
	}
	explicitImageIntent := IsExplicitImageGenerationIntent(openAIResponsesEndpoint, model, firstMessage)
	if explicitImageIntent {
		return nil, fmt.Errorf("image ws requires Go")
	}
	if bytes.Contains(firstMessage, []byte("function_call_output")) {
		return nil, fmt.Errorf("function_call_output requires Go WS state")
	}
	policyModel := openAIWSPassthroughPolicyModelForFrame(account, firstMessage)
	updatedFirst, blocked, err := s.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, policyModel, firstMessage)
	if err != nil {
		return nil, err
	}
	if blocked != nil {
		return nil, blocked
	}
	firstMessage = updatedFirst
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		isolatedFirst, isolated, err := applyOpenAIUpstreamStrongIsolationWSBody(firstMessage, true)
		if err != nil {
			return nil, fmt.Errorf("apply upstream strong isolation: %w", err)
		}
		if isolated {
			firstMessage = isolatedFirst
		}
	}
	optimizedFirst, cacheCreationOptimization, err := s.ApplyOpenAIPromptCacheCreationOptimizationBodyWithExplicitIntent(account, policyModel, firstMessage, explicitImageIntent)
	if err != nil {
		return nil, err
	}
	firstMessage = optimizedFirst
	cacheCreationMode, cacheCreationModel := openAIEdgePromptCacheCreationOptimizationFields(account, policyModel, cacheCreationOptimization)
	// An edge WS session may change models after the first response.create via
	// session.update. Keep the account policy available to edge-rs even when the
	// initial model is not GPT-5.6 so later eligible turns are handled correctly.
	if cacheCreationMode == "" && account.IsOpenAIPromptCacheCreationOptimizationEnabled() {
		cacheCreationMode = account.OpenAIPromptCacheCreationOptimizationMode()
		cacheCreationModel = policyModel
	}

	promptCacheKey := strings.TrimSpace(gjson.GetBytes(firstMessage, "prompt_cache_key").String())
	isCodexCLI := false
	if c != nil {
		isCodexCLI = openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator"))
	}
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		isCodexCLI = true
	}
	headers, _, headerErr := s.buildOpenAIWSHeaders(c, account, token, wsDecision, isCodexCLI, "", "", promptCacheKey)
	if headerErr != nil {
		return nil, headerErr
	}
	headerMap := make(map[string]string, len(headers))
	for key, values := range headers {
		if len(values) > 0 {
			headerMap[key] = values[0]
		}
	}
	if account.IsOpenAIUpstreamStrongIsolationEnabled() {
		scrubOpenAIEdgeStrongIsolationHeaders(headerMap)
	}
	wsURL, err := s.buildOpenAIResponsesWSURL(account)
	if err != nil {
		return nil, err
	}
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(firstMessage, model)
	serviceTier := extractOpenAIServiceTierFromBody(firstMessage)
	return &OpenAIEdgePreparedChatCompletions{
		Plan: OpenAIEdgePlan{
			Action:                                 OpenAIEdgeActionRelay,
			AccountID:                              account.ID,
			AccountType:                            account.Type,
			Transport:                              OpenAIEdgeTransportWSV2,
			ResponseDialect:                        OpenAIEdgeDialectResponses,
			UpstreamURL:                            wsURL,
			Headers:                                headerMap,
			Body:                                   json.RawMessage(firstMessage),
			BodyRawBase64:                          EncodeOpenAIEdgeRawBody(firstMessage),
			ProxyURL:                               proxyURL,
			PromptCacheCreationOptimizationMode:    cacheCreationMode,
			PromptCacheCreationOptimizationModel:   cacheCreationModel,
			PromptCacheCreationOptimizationApplied: cacheCreationOptimization.Applied,
		},
		Model:           model,
		BillingModel:    model,
		UpstreamModel:   model,
		ReasoningEffort: reasoningEffort,
		ServiceTier:     serviceTier,
	}, nil
}

func openAIEdgePromptCacheCreationOptimizationFields(
	account *Account,
	model string,
	result openAIPromptCacheCreationOptimizationResult,
) (string, string) {
	if !result.Applied || account == nil {
		return "", ""
	}
	return account.OpenAIPromptCacheCreationOptimizationMode(), model
}

func (s *OpenAIGatewayService) checkOpenAIEdgeLocalAccountPolicy(ctx context.Context, c *gin.Context, account *Account, body []byte) error {
	if s == nil {
		return fmt.Errorf("openai gateway service is nil")
	}
	restrictionResult := s.detectCodexClientRestriction(c, account, body)
	apiKeyID := getAPIKeyIDFromContext(c)
	logCodexCLIOnlyDetection(ctx, c, account, apiKeyID, restrictionResult, body)
	if restrictionResult.Enabled && !restrictionResult.Matched {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		return fmt.Errorf("local account policy denied: %s", strings.TrimSpace(restrictionResult.Reason))
	}
	return nil
}
