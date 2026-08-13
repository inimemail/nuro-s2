package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/websearch"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func grokTargetForRequest(c *gin.Context, apiKey *service.APIKey) bool {
	if apiKey == nil || apiKey.Group == nil {
		return false
	}
	if apiKey.Group.Platform == service.PlatformGrok {
		return true
	}
	p, ok := service.ResolvedTargetPlatformFromContext(c.Request.Context())
	return apiKey.Group.Platform == service.PlatformComposite && ok && p == service.PlatformGrok
}

func (h *OpenAIGatewayHandler) GrokVoice(c *gin.Context, endpoint string) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || !grokTargetForRequest(c, apiKey) {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Voice API is not supported for this platform")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if upstreamModel, ok := service.ResolvedUpstreamModelFromContext(c.Request.Context()); ok && len(body) > 0 {
		body = service.ReplaceModelInBody(body, upstreamModel)
	}
	h.handleGrokAuxHTTP(c, apiKey, subject, endpoint, body, func(account *service.Account) (*service.OpenAIForwardResult, error) {
		return h.gatewayService.ForwardGrokVoice(c.Request.Context(), c, account, endpoint, body, c.GetHeader("Content-Type"))
	})
}

func (h *OpenAIGatewayHandler) GrokWebSearch(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || !grokTargetForRequest(c, apiKey) {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Web search is not supported for this platform")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil || len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is required")
		return
	}
	query := strings.TrimSpace(gjson.GetBytes(body, "query").String())
	if query == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "query is required")
		return
	}
	maxResults := normalizeGrokWebSearchMaxResults(int(gjson.GetBytes(body, "max_results").Int()))
	h.handleGrokAuxHTTP(c, apiKey, subject, "web_search", body, func(account *service.Account) (*service.OpenAIForwardResult, error) {
		return h.gatewayService.ForwardGrokWebSearch(c.Request.Context(), c, account, body)
	}, func(result *service.OpenAIForwardResult) {
		c.JSON(http.StatusOK, gin.H{
			"query":       query,
			"results":     extractGrokWebSearchSources(result.ResponseBody, maxResults),
			"provider":    "grok-native",
			"max_results": maxResults,
		})
	})
}

// GrokXSearch exposes xAI's native X search through the same protected
// scheduler/account/billing path as web_search. The endpoint is intentionally
// separate so callers cannot silently change the existing web search contract.
func (h *OpenAIGatewayHandler) GrokXSearch(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || !grokTargetForRequest(c, apiKey) {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "X search is not supported for this platform")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	body, err := io.ReadAll(c.Request.Body)
	if err != nil || len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is required")
		return
	}
	query := strings.TrimSpace(gjson.GetBytes(body, "query").String())
	if query == "" {
		query = strings.TrimSpace(gjson.GetBytes(body, "input").String())
	}
	if query == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "query is required")
		return
	}
	maxResults := normalizeGrokWebSearchMaxResults(int(gjson.GetBytes(body, "max_results").Int()))
	h.handleGrokAuxHTTP(c, apiKey, subject, "x_search", body, func(account *service.Account) (*service.OpenAIForwardResult, error) {
		return h.gatewayService.ForwardGrokXSearch(c.Request.Context(), c, account, body)
	}, func(result *service.OpenAIForwardResult) {
		c.JSON(http.StatusOK, gin.H{
			"query":       query,
			"results":     extractGrokWebSearchSources(result.ResponseBody, maxResults),
			"provider":    "grok-native",
			"max_results": maxResults,
		})
	})
}

func (h *OpenAIGatewayHandler) handleGrokAuxHTTP(c *gin.Context, apiKey *service.APIKey, subject middleware2.AuthSubject, endpoint string, body []byte, forward func(*service.Account) (*service.OpenAIForwardResult, error), onSuccess ...func(*service.OpenAIForwardResult)) {
	if !h.ensureResponsesDependencies(c, nil) {
		return
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}
	streamStarted := false
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, requestLogger(c, "handler.openai_gateway.grok_aux"))
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	failed := make(map[int64]struct{})
	requestedModel := service.DefaultGrokSearchBillingModel
	if endpoint != "web_search" && endpoint != "x_search" {
		requestedModel = "grok-voice-latest"
	}
	if upstreamModel, ok := service.ResolvedUpstreamModelFromContext(c.Request.Context()); ok {
		requestedModel = upstreamModel
	}
	for attempt := 0; attempt < 4; attempt++ {
		selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(c.Request.Context(), apiKey.GroupID, "", "", requestedModel, failed, service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityChatCompletions, false, service.PlatformGrok, -1)
		if err != nil || selection == nil || selection.Account == nil {
			h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "No available Grok accounts")
			return
		}
		account := selection.Account
		slot := h.acquireResponsesAccountSlot(c, apiKey.GroupID, "", selection, false, &streamStarted, requestLogger(c, "handler.openai_gateway.grok_aux"))
		if !slot.Acquired {
			if slot.CapacityMiss {
				failed[account.ID] = struct{}{}
				continue
			}
			return
		}
		result, forwardErr := func() (*service.OpenAIForwardResult, error) {
			if slot.ReleaseFunc != nil {
				defer slot.ReleaseFunc()
			}
			return forward(account)
		}()
		if forwardErr == nil {
			h.recordGrokAuxUsage(c, apiKey, account, subscription, endpoint, body, result)
			if len(onSuccess) > 0 && onSuccess[0] != nil {
				onSuccess[0](result)
			}
			return
		}
		var failover *service.UpstreamFailoverError
		if errors.As(forwardErr, &failover) && failover.ShouldRetryNextAccount() {
			failed[account.ID] = struct{}{}
			continue
		}
		if !service.IsResponseCommitted(c) && !c.Writer.Written() && c.Request.Context().Err() == nil {
			h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		}
		return
	}
	if !service.IsResponseCommitted(c) && !c.Writer.Written() && c.Request.Context().Err() == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "No available Grok accounts")
	}
}

const (
	defaultGrokWebSearchResults = 5
	maxGrokWebSearchResults     = 20
)

func normalizeGrokWebSearchMaxResults(value int) int {
	if value <= 0 {
		return defaultGrokWebSearchResults
	}
	if value > maxGrokWebSearchResults {
		return maxGrokWebSearchResults
	}
	return value
}

func extractGrokWebSearchSources(body []byte, maxResults int) []websearch.SearchResult {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return []websearch.SearchResult{}
	}
	maxResults = normalizeGrokWebSearchMaxResults(maxResults)
	sources := make(map[string]websearch.SearchResult)
	order := make([]string, 0, maxResults)
	add := func(rawURL, title, snippet string) {
		key, ok := normalizeGrokWebSearchURL(rawURL)
		if !ok {
			return
		}
		item, exists := sources[key]
		if !exists {
			item.URL = strings.TrimSpace(rawURL)
			order = append(order, key)
		}
		if item.Title == "" {
			item.Title = strings.TrimSpace(title)
		}
		if item.Snippet == "" {
			item.Snippet = strings.TrimSpace(snippet)
		}
		sources[key] = item
	}
	output := gjson.GetBytes(body, "output")
	output.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "web_search_call" || item.Get("type").String() == "x_search_call" {
			item.Get("action.sources").ForEach(func(_, source gjson.Result) bool {
				add(source.Get("url").String(), source.Get("title").String(), source.Get("snippet").String())
				return true
			})
		}
		if item.Get("type").String() == "message" {
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				part.Get("annotations").ForEach(func(_, annotation gjson.Result) bool {
					add(annotation.Get("url").String(), annotation.Get("title").String(), "")
					return true
				})
				return true
			})
		}
		return true
	})

	capacity := len(order)
	if capacity > maxResults {
		capacity = maxResults
	}
	result := make([]websearch.SearchResult, 0, capacity)
	for _, key := range order {
		if len(result) >= maxResults {
			break
		}
		item := sources[key]
		if item.Title == "" {
			if parsed, err := url.Parse(item.URL); err == nil {
				item.Title = strings.TrimPrefix(strings.ToLower(parsed.Host), "www.")
			}
		}
		result = append(result, item)
	}
	return result
}

func normalizeGrokWebSearchURL(raw string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed.String(), true
}

func (h *OpenAIGatewayHandler) recordGrokAuxUsage(c *gin.Context, apiKey *service.APIKey, account *service.Account, subscription *service.UserSubscription, endpoint string, body []byte, result *service.OpenAIForwardResult) {
	if result == nil || (result.AudioUsage == nil && result.SearchCount <= 0) {
		return
	}
	parent := c.Request.Context()
	quotaPlatform := service.QuotaPlatform(parent, apiKey)
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	sessionID := service.ExtractClientSessionID(c)
	requestPayloadHash := service.HashUsageRequestPayload(body)
	channelUsageFields := service.ChannelUsageFields{OriginalModel: result.Model, ChannelMappedModel: result.UpstreamModel}
	h.submitMandatoryUsageRecordTask(parent, func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{Result: result, APIKey: apiKey, User: apiKey.User, Account: account, Subscription: subscription, QuotaPlatform: quotaPlatform, InboundEndpoint: inboundEndpoint, UpstreamEndpoint: upstreamEndpoint, UserAgent: userAgent, IPAddress: clientIP, SessionID: sessionID, RequestPayloadHash: requestPayloadHash, APIKeyService: h.apiKeyService, ChannelUsageFields: channelUsageFields}); err != nil {
			logger.L().Error("grok_aux.record_usage_failed", zap.String("endpoint", endpoint), zap.Error(err))
		}
	})
}

func (h *OpenAIGatewayHandler) GrokRealtime(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || !grokTargetForRequest(c, apiKey) {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Realtime API is not supported for this platform")
		return
	}
	if !isOpenAIWSUpgradeRequest(c.Request) {
		h.errorResponse(c, http.StatusUpgradeRequired, "invalid_request_error", "WebSocket upgrade required")
		return
	}
	if !h.ensureResponsesDependencies(c, nil) {
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		return
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}
	streamStarted := false
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, true, &streamStarted, requestLogger(c, "handler.openai_gateway.grok_realtime"))
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	realtimeModel := firstNonEmptyString(c.Query("model"), "grok-voice-latest")
	if upstreamModel, resolved := service.ResolvedUpstreamModelFromContext(c.Request.Context()); resolved {
		realtimeModel = upstreamModel
	}
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(c.Request.Context(), apiKey.GroupID, "", "", realtimeModel, nil, service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityChatCompletions, false, service.PlatformGrok, -1)
	if err != nil || selection == nil || selection.Account == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "No available Grok accounts")
		return
	}
	slot := h.acquireResponsesAccountSlot(c, apiKey.GroupID, "", selection, true, &streamStarted, requestLogger(c, "handler.openai_gateway.grok_realtime"))
	if !slot.Acquired {
		return
	}
	if slot.ReleaseFunc != nil {
		defer slot.ReleaseFunc()
	}
	token, _, err := h.gatewayService.GetAccessToken(c.Request.Context(), selection.Account)
	if err != nil {
		h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Grok credential unavailable")
		return
	}
	conn, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	started := time.Now()
	audioObserved, proxyErr := h.gatewayService.ProxyGrokRealtime(c.Request.Context(), conn, selection.Account, token, realtimeModel)
	if proxyErr != nil && !isExpectedGrokRealtimeClose(proxyErr) {
		_ = conn.Close(coderws.StatusInternalError, "upstream realtime websocket failed")
		return
	}
	if audioObserved && time.Since(started) > 0 {
		elapsed := time.Since(started)
		result := &service.OpenAIForwardResult{RequestID: service.StableGrokRealtimeBillingRequestID(""), Model: firstNonEmptyString(c.Query("model"), "grok-voice-latest"), UpstreamModel: realtimeModel, Duration: elapsed, AudioUsage: &service.AudioUsage{Mode: "realtime", DurationOrUnits: elapsed.Minutes()}}
		h.recordGrokAuxUsage(c, apiKey, selection.Account, subscription, "realtime", []byte(result.Model), result)
	}
	_ = subject
}

func isExpectedGrokRealtimeClose(err error) bool {
	if err == nil {
		return true
	}
	switch coderws.CloseStatus(err) {
	case coderws.StatusNormalClosure, coderws.StatusGoingAway, coderws.StatusNoStatusRcvd, coderws.StatusAbnormalClosure:
		return true
	default:
		return false
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
