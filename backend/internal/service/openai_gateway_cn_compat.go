package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// doOpenAIUpstream deliberately stays on the existing local transport. CN
// support must not pull the upstream plugin system into this fork.
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if request != nil && request.URL != nil {
		applyOpenCodeUpstreamUserAgent(account, request.URL.String(), request.Header)
	}
	defer scheduleOpenCodeGoUsageActivity(s.deferredService, account)
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
	if usage == nil || !gjson.Valid(data) {
		return
	}
	parsed := gjson.Parse(data)
	switch parsed.Get("type").String() {
	case "message_start":
		mergeSSEUsagePatch(usage, anthropicUsagePatchFromNode(parsed.Get("message.usage"), true))
	case "message_delta":
		mergeSSEUsagePatch(usage, anthropicUsagePatchFromNode(parsed.Get("usage"), false))
	default:
		patch := anthropicUsagePatchFromNode(parsed.Get("usage"), false)
		if patch != nil {
			patch.hasInputTokens, patch.hasOutputTokens = false, false
			mergeSSEUsagePatch(usage, patch)
		}
	}
}

func buildOpenAIResponsesURLForPlatform(platform string, base string) string {
	if platform == PlatformDeepSeek || platform == PlatformKimi || platform == PlatformMiniMax {
		return buildOpenAIEndpointURL(base, "/responses")
	}
	return buildOpenAIResponsesURL(base)
}

func normalizeDeepSeekResponsesRequestBody(account *Account, body []byte) []byte {
	if account == nil {
		return body
	}
	native := account.IsDeepSeek() && (account.GetAPIProtocol() == APIProtocolResponses || account.IsAdaptiveAPIProtocol())
	target, _ := url.Parse(account.GetBaseURL())
	images := native || (account.IsOpenAICompatible() && target != nil && strings.EqualFold(target.Hostname(), "api.deepseek.com"))
	if !native && !images {
		return body
	}
	var request map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return body
	}
	if native {
		request["store"] = false
		delete(request, "previous_response_id")
	}
	if input, changed := apicompat.LiftResponsesToolOutputMedia(request["input"]); changed {
		request["input"] = input
	}
	if images {
		aliasDeepSeekResponsesImages(request["input"])
	}
	rebuilt, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return rebuilt
}

// Only known content containers are visited; tool arguments are never rewritten.
func aliasDeepSeekResponsesImages(input any) {
	switch value := input.(type) {
	case []any:
		for _, item := range value {
			aliasDeepSeekResponsesImages(item)
		}
	case map[string]any:
		if value["type"] == "input_image" || value["type"] == "image_url" || value["type"] == "image" {
			imageURL, _ := value["image_url"].(string)
			if imageURL == "" {
				if image, ok := value["image_url"].(map[string]any); ok {
					imageURL, _ = image["url"].(string)
				}
			}
			if imageURL == "" {
				imageURL, _ = value["url"].(string)
			}
			if imageURL == "" {
				if source, ok := value["source"].(map[string]any); ok {
					if source["type"] == "url" {
						imageURL, _ = source["url"].(string)
					}
					if source["type"] == "base64" {
						media, _ := source["media_type"].(string)
						data, _ := source["data"].(string)
						if strings.HasPrefix(media, "image/") && data != "" {
							imageURL = "data:" + media + ";base64," + data
						}
					}
				}
			}
			if imageURL != "" {
				value["type"] = "input_image"
				delete(value, "url")
				value["image_url"] = imageURL
			}
		}
		aliasDeepSeekResponsesImages(value["content"])
		aliasDeepSeekResponsesImages(value["output"])
	}
}
