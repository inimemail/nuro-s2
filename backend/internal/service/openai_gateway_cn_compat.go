package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// doOpenAIUpstream deliberately stays on the existing local transport. CN
// support must not pull the upstream plugin system into this fork.
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	return s.httpUpstream.DoWithTLS(request, proxyURL, account.ID, account.Concurrency, s.resolveTLSProfile(account))
}

func (s *OpenAIGatewayService) readOpenAIUpstreamError(resp *http.Response) ([]byte, string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	message := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(body)))
	return body, message
}

// failoverOpenAIUpstreamHTTPError uses the existing account-switch decision.
// It introduces no retry, reconnect, or deadline reset of its own.
func (s *OpenAIGatewayService) failoverOpenAIUpstreamHTTPError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	body []byte,
	message string,
	model string,
) *UpstreamFailoverError {
	if account == nil || !s.shouldFailoverOpenAIAccountResponse(ctx, account, resp.StatusCode, message, body) {
		return nil
	}
	decision := s.classifyOpenAIPoolFailover(ctx, account, resp.StatusCode, message, body)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:   opsUpstreamProxyID(account),
		ProxyName: opsUpstreamProxyName(account),
		Platform:  account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Kind: "failover", Message: message,
	})
	s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, model)
	return &UpstreamFailoverError{
		StatusCode: resp.StatusCode, ResponseBody: body, Message: message,
		ProbeModel: strings.TrimSpace(model), ProbeKind: openAIPoolProbeKindForModel(model),
		RetryableOnSameAccount: decision.RetryableOnSameAccount,
		RetryRuleKey:           decision.RetryRuleKey, RetryRuleLimit: decision.RetryRuleLimit,
		SkipPoolSoftCooldown: decision.SkipSoftCooldown,
	}
}

func adaptResponsesClientToolsForAnthropic(body []byte) ([]byte, apicompat.ResponsesClientToolMapping, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var requestBody map[string]any
	if err := decoder.Decode(&requestBody); err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, err
	}
	additionalChanged, err := liftResponsesAdditionalToolsForCN(requestBody)
	if err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, err
	}
	mapping, changed, err := apicompat.AdaptResponsesClientTools(requestBody)
	if err != nil {
		return body, apicompat.ResponsesClientToolMapping{}, err
	}
	if !changed && !additionalChanged {
		return body, mapping, nil
	}
	rebuilt, err := json.Marshal(requestBody)
	return rebuilt, mapping, err
}

func liftResponsesAdditionalToolsForCN(requestBody map[string]any) (bool, error) {
	input, ok := requestBody["input"].([]any)
	if !ok {
		return false, nil
	}
	tools, _ := requestBody["tools"].([]any)
	kept := make([]any, 0, len(input))
	changed := false
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(item["type"])) != "additional_tools" {
			kept = append(kept, raw)
			continue
		}
		additional, ok := item["tools"].([]any)
		if !ok {
			return false, fmt.Errorf("additional_tools.tools must be an array")
		}
		tools = append(tools, additional...)
		changed = true
	}
	if changed {
		requestBody["tools"] = tools
		requestBody["input"] = kept
	}
	return changed, nil
}

func invalidNonStreamingJSONFailoverError(
	ctx context.Context,
	rateLimitService *RateLimitService,
	resp *http.Response,
	account *Account,
	body []byte,
	parseErr error,
	requestedModel ...string,
) error {
	const statusCode = http.StatusBadGateway
	retryable := false
	if account != nil {
		retryable = account.IsPoolMode() && account.IsPoolModeRetryableStatus(statusCode)
		logger.LegacyPrintf("service.gateway", "Account %d(%s): upstream returned non-JSON 2xx response: %v", account.ID, account.Name, parseErr)
		if rateLimitService != nil {
			rateLimitService.HandleUpstreamError(ctx, account, statusCode, resp.Header, body, requestedModel...)
		}
	}
	return &UpstreamFailoverError{
		StatusCode: statusCode, ResponseBody: body, ResponseHeaders: resp.Header,
		RetryableOnSameAccount: retryable,
	}
}

// parseSSEUsagePassthrough normalizes native Anthropic fields and the aliases
// returned by Kimi-compatible endpoints into mutually exclusive buckets.
func parseSSEUsagePassthrough(data string, usage *ClaudeUsage) {
	if usage == nil || data == "" || data == "[DONE]" {
		return
	}
	parsed := gjson.Parse(data)
	usageNode := parsed.Get("usage")
	if parsed.Get("type").String() == "message_start" {
		usageNode = parsed.Get("message.usage")
	}
	if !usageNode.Exists() {
		return
	}
	if parsed.Get("type").String() == "message_start" {
		usage.InputTokens = int(usageNode.Get("input_tokens").Int())
		usage.CacheCreationInputTokens = int(usageNode.Get("cache_creation_input_tokens").Int())
		usage.CacheReadInputTokens = int(usageNode.Get("cache_read_input_tokens").Int())
	} else if parsed.Get("type").String() == "message_delta" {
		if value := usageNode.Get("input_tokens").Int(); value > 0 {
			usage.InputTokens = int(value)
		}
		if value := usageNode.Get("output_tokens").Int(); value > 0 {
			usage.OutputTokens = int(value)
		}
		if value := usageNode.Get("cache_creation_input_tokens").Int(); value > 0 {
			usage.CacheCreationInputTokens = int(value)
		}
		if value := usageNode.Get("cache_read_input_tokens").Int(); value > 0 {
			usage.CacheReadInputTokens = int(value)
		}
	}
	cc5m := int(usageNode.Get("cache_creation.ephemeral_5m_input_tokens").Int())
	cc1h := int(usageNode.Get("cache_creation.ephemeral_1h_input_tokens").Int())
	if cc5m > 0 || cc1h > 0 {
		usage.CacheCreation5mTokens = cc5m
		usage.CacheCreation1hTokens = cc1h
		if usage.CacheCreationInputTokens == 0 {
			usage.CacheCreationInputTokens = cc5m + cc1h
		}
	}
	normalizeAnthropicCompatiblePromptUsage(usageNode, usage)
}

func normalizeAnthropicCompatiblePromptUsage(node gjson.Result, usage *ClaudeUsage) bool {
	if usage == nil || !node.Exists() {
		return false
	}
	prompt := node.Get("prompt_tokens")
	hit := node.Get("prompt_cache_hit_tokens")
	miss := node.Get("prompt_cache_miss_tokens")
	if (!prompt.Exists() || prompt.Int() <= 0) && !hit.Exists() && !miss.Exists() {
		return false
	}
	cacheRead := usage.CacheReadInputTokens
	for _, path := range []string{"cache_read_input_tokens", "cached_tokens", "prompt_tokens_details.cached_tokens"} {
		if value := node.Get(path); cacheRead == 0 && value.Exists() {
			cacheRead = max(int(value.Int()), 0)
		}
	}
	if cacheRead == 0 && hit.Exists() {
		cacheRead = max(int(hit.Int()), 0)
	}
	cacheCreation := usage.CacheCreationInputTokens
	if miss.Exists() {
		usage.InputTokens = max(int(miss.Int()), 0)
	} else {
		usage.InputTokens = max(int(prompt.Int())-cacheRead-cacheCreation, 0)
	}
	usage.CacheReadInputTokens = cacheRead
	usage.CacheCreationInputTokens = cacheCreation
	return true
}

func buildOpenAIResponsesURLForPlatform(platform string, base string) string {
	if platform == PlatformDeepSeek || platform == PlatformKimi {
		return buildOpenAIEndpointURL(base, "/responses")
	}
	return buildOpenAIResponsesURL(base)
}

func normalizeDeepSeekResponsesRequestBody(account *Account, body []byte) []byte {
	if account == nil || !account.IsDeepSeek() ||
		(account.GetAPIProtocol() != APIProtocolResponses && !account.IsAdaptiveAPIProtocol()) {
		return body
	}
	var request map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return body
	}
	request["store"] = false
	delete(request, "previous_response_id")
	rebuilt, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return rebuilt
}
