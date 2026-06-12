package handler

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const openAIEdgeSecretHeader = "X-Sub2API-Edge-Secret"

type openAIEdgeLease struct {
	mu                 sync.Mutex
	edgeRequestID      string
	leaseID            string
	createdAt          time.Time
	expiresAt          time.Time
	releaseOnce        sync.Once
	userReleaseFunc    func()
	accountReleaseFunc func()
	apiKey             *service.APIKey
	subject            middleware2.AuthSubject
	subscription       *service.UserSubscription
	account            *service.Account
	requestBody        []byte
	forwardBody        []byte
	sessionHash        string
	failedAccountIDs   map[int64]struct{}
	sameAccountRetries map[int64]int
	switchCount        int
	maxAccountSwitches int
	routingModel       string
	requestModel       string
	billingModel       string
	upstreamModel      string
	reasoningEffort    *string
	serviceTier        *string
	userAgent          string
	clientIP           string
	inboundEndpoint    string
	upstreamEndpoint   string
	requestPayloadHash string
	channelUsageFields service.ChannelUsageFields
	timer              *time.Timer
}

func (l *openAIEdgeLease) release() {
	if l == nil {
		return
	}
	l.releaseOnce.Do(func() {
		l.mu.Lock()
		timer := l.timer
		accountReleaseFunc := l.accountReleaseFunc
		userReleaseFunc := l.userReleaseFunc
		l.timer = nil
		l.accountReleaseFunc = nil
		l.userReleaseFunc = nil
		l.mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}
		if userReleaseFunc != nil {
			userReleaseFunc()
		}
	})
}

func (l *openAIEdgeLease) releaseAccount() {
	if l == nil {
		return
	}
	l.mu.Lock()
	accountReleaseFunc := l.accountReleaseFunc
	l.accountReleaseFunc = nil
	l.mu.Unlock()
	if accountReleaseFunc != nil {
		accountReleaseFunc()
	}
}

func (l *openAIEdgeLease) openAIRoutingModel() string {
	if l == nil {
		return ""
	}
	if model := strings.TrimSpace(l.routingModel); model != "" {
		return model
	}
	return l.requestModel
}

func (h *OpenAIGatewayHandler) openAIEdgeConfig() config.GatewayOpenAIEdgeRSConfig {
	if h == nil || h.cfg == nil {
		return config.GatewayOpenAIEdgeRSConfig{}
	}
	return h.cfg.Gateway.OpenAIEdgeRS
}

func openAIEdgeFallbackPlan(req service.OpenAIEdgePrepareRequest, reason string, cfg config.GatewayOpenAIEdgeRSConfig, lowLatencyMode string) service.OpenAIEdgePlan {
	return service.OpenAIEdgePlan{
		Action:         service.OpenAIEdgeActionFallbackGo,
		Reason:         reason,
		EdgeRequestID:  req.EdgeRequestID,
		LeaseTTLMS:     cfg.LeaseTTLMS,
		LowLatencyMode: lowLatencyMode,
	}
}

func (h *OpenAIGatewayHandler) openAIEdgeLowLatencyMode() string {
	if h == nil || h.cfg == nil {
		return ""
	}
	return h.cfg.StreamLowLatencyMode()
}

func (h *OpenAIGatewayHandler) storeOpenAIEdgeLease(lease *openAIEdgeLease, ttl time.Duration) {
	if h == nil || lease == nil || lease.leaseID == "" {
		return
	}
	h.openAIEdgeLeaseMu.Lock()
	if h.openAIEdgeLeases == nil {
		h.openAIEdgeLeases = make(map[string]*openAIEdgeLease)
	}
	h.openAIEdgeLeases[lease.leaseID] = lease
	h.openAIEdgeLeaseMu.Unlock()
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	lease.timer = time.AfterFunc(ttl, func() {
		h.openAIEdgeLeaseMu.Lock()
		current := h.openAIEdgeLeases[lease.leaseID]
		if current == lease {
			delete(h.openAIEdgeLeases, lease.leaseID)
		}
		h.openAIEdgeLeaseMu.Unlock()
		lease.release()
	})
}

func (h *OpenAIGatewayHandler) takeOpenAIEdgeLease(leaseID string) *openAIEdgeLease {
	if h == nil || strings.TrimSpace(leaseID) == "" {
		return nil
	}
	h.openAIEdgeLeaseMu.Lock()
	defer h.openAIEdgeLeaseMu.Unlock()
	lease := h.openAIEdgeLeases[leaseID]
	if lease != nil {
		delete(h.openAIEdgeLeases, leaseID)
	}
	return lease
}

func (h *OpenAIGatewayHandler) getOpenAIEdgeLease(leaseID string) *openAIEdgeLease {
	if h == nil || strings.TrimSpace(leaseID) == "" {
		return nil
	}
	h.openAIEdgeLeaseMu.Lock()
	defer h.openAIEdgeLeaseMu.Unlock()
	return h.openAIEdgeLeases[leaseID]
}

func (h *OpenAIGatewayHandler) requireOpenAIEdgeSecret(c *gin.Context) bool {
	cfg := h.openAIEdgeConfig()
	expected := strings.TrimSpace(cfg.InternalSecret)
	if !cfg.InternalAPIEnabled || expected == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "openai edge internal api is disabled"})
		return false
	}
	got := strings.TrimSpace(c.GetHeader(openAIEdgeSecretHeader))
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid edge secret"})
		return false
	}
	return true
}

func (h *OpenAIGatewayHandler) OpenAIEdgePrepare(c *gin.Context) {
	if !h.requireOpenAIEdgeSecret(c) {
		return
	}
	var req service.OpenAIEdgePrepareRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid prepare request"})
		return
	}
	if err := normalizeOpenAIEdgePrepareBody(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid prepare raw body"})
		return
	}

	cfg := h.openAIEdgeConfig()
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if !cfg.Enabled || mode == "" || mode == "off" {
		c.JSON(http.StatusOK, openAIEdgeFallbackPlan(req, "edge_disabled", cfg, h.openAIEdgeLowLatencyMode()))
		return
	}
	if mode == "shadow" {
		c.JSON(http.StatusOK, openAIEdgeFallbackPlan(req, "edge_shadow_mode", cfg, h.openAIEdgeLowLatencyMode()))
		return
	}

	var (
		plan service.OpenAIEdgePlan
		ok   bool
	)
	switch normalizeOpenAIEdgePath(req.Path) {
	case "/v1/chat/completions":
		plan, ok = h.prepareOpenAIEdgeRawChatRelay(c, req, cfg)
	case "/v1/responses":
		plan, ok = h.prepareOpenAIEdgeRawResponsesRelay(c, req, cfg)
	default:
		c.JSON(http.StatusOK, openAIEdgeFallbackPlan(req, "edge_route_not_supported", cfg, h.openAIEdgeLowLatencyMode()))
		return
	}
	if !ok {
		return
	}
	c.JSON(http.StatusOK, plan)
}

func (h *OpenAIGatewayHandler) prepareOpenAIEdgeRawChatRelay(c *gin.Context, req service.OpenAIEdgePrepareRequest, cfg config.GatewayOpenAIEdgeRSConfig) (service.OpenAIEdgePlan, bool) {
	fallback := func(reason string) (service.OpenAIEdgePlan, bool) {
		c.JSON(http.StatusOK, openAIEdgeFallbackPlan(req, reason, cfg, h.openAIEdgeLowLatencyMode()))
		return service.OpenAIEdgePlan{}, false
	}
	if h == nil || h.gatewayService == nil || h.billingCacheService == nil || h.apiKeyService == nil || h.concurrencyHelper == nil {
		return fallback("edge_dependencies_missing")
	}
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodPost) || normalizeOpenAIEdgePath(req.Path) != "/v1/chat/completions" {
		return fallback("edge_route_not_supported")
	}
	if req.Stream == nil || !*req.Stream || !gjson.GetBytes(req.Body, "stream").Bool() {
		return fallback("edge_only_stream_chat_supported")
	}
	if len(req.Body) == 0 || !gjson.ValidBytes(req.Body) {
		return fallback("invalid_json_body")
	}
	reqModel := strings.TrimSpace(gjson.GetBytes(req.Body, "model").String())
	if reqModel == "" {
		return fallback("missing_model")
	}
	applyOpenAIEdgeClientHeaders(c, req.Headers)
	apiKey, subject, subscription, reason := h.authenticateOpenAIEdgeClient(c, req)
	if reason != "" {
		return fallback(reason)
	}
	if !cfg.RelayChatCompletions {
		return fallback("edge_chat_completions_relay_disabled")
	}
	if reason := openAIEdgeRolloutRejectReason(cfg, apiKey, reqModel, req.EdgeRequestID); reason != "" {
		return fallback(reason)
	}
	reqLog := requestLogger(c, "handler.openai_edge.prepare",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("model", reqModel),
	)
	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIChat, reqModel, req.Body); decision != nil && decision.Blocked {
		return fallback("content_moderation_blocked")
	}
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardBody := []byte(req.Body)
	if channelMapping.Mapped {
		forwardBody = h.gatewayService.ReplaceModelInBody(forwardBody, channelMapping.MappedModel)
	}
	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(c.Request.Context(), subject.UserID, subject.Concurrency)
	if err != nil {
		reqLog.Warn("openai_edge.user_slot_acquire_failed", zap.Error(err))
		return fallback("user_slot_acquire_failed")
	}
	if !userAcquired {
		return fallback("user_slot_busy")
	}
	releaseUserOnFailure := true
	defer func() {
		if releaseUserOnFailure && userReleaseFunc != nil {
			userReleaseFunc()
		}
	}()
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_edge.billing_check_failed", zap.Error(err))
		return fallback("billing_check_failed")
	}
	sessionHash := h.gatewayService.GeneratePromptCacheBoostAffinitySessionHash(c, req.Body, reqModel)
	if sessionHash == "" {
		sessionHash = h.gatewayService.GenerateSessionHash(c, req.Body)
	}
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
		c.Request.Context(),
		apiKey.GroupID,
		"",
		sessionHash,
		reqModel,
		nil,
		service.OpenAIUpstreamTransportAny,
		service.OpenAIEndpointCapabilityChatCompletions,
		false,
	)
	if err != nil || selection == nil || selection.Account == nil {
		if err != nil {
			reqLog.Warn("openai_edge.account_select_failed", zap.Error(err))
		}
		return fallback("account_select_failed")
	}
	account := selection.Account
	sessionHash = h.gatewayService.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, account)
	sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
	if !service.IsOpenAIEdgeRawChatRelayEligible(account) {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return fallback("account_requires_go_transform")
	}
	accountReleaseFunc := selection.ReleaseFunc
	if !selection.Acquired {
		if selection.WaitPlan == nil {
			return fallback("account_slot_not_available")
		}
		var accountAcquired bool
		accountReleaseFunc, accountAcquired, err = h.concurrencyHelper.TryAcquireAccountSlot(c.Request.Context(), account.ID, selection.WaitPlan.MaxConcurrency)
		if err != nil {
			reqLog.Warn("openai_edge.account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			return fallback("account_slot_acquire_failed")
		}
		if !accountAcquired {
			return fallback("account_slot_busy")
		}
		if err := h.gatewayService.BindStickySession(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai_edge.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
	}
	releaseAccountOnFailure := true
	defer func() {
		if releaseAccountOnFailure && accountReleaseFunc != nil {
			accountReleaseFunc()
		}
	}()
	prepared, err := h.gatewayService.BuildRawChatCompletionsEdgePlan(c.Request.Context(), c, account, forwardBody, "")
	if err != nil {
		reqLog.Warn("openai_edge.build_raw_chat_plan_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return fallback("build_raw_chat_plan_failed")
	}
	leaseID := "lease_" + uuid.NewString()
	edgeRequestID := strings.TrimSpace(req.EdgeRequestID)
	if edgeRequestID == "" {
		edgeRequestID = "edge_" + uuid.NewString()
	}
	ttl := time.Duration(cfg.LeaseTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	plan := prepared.Plan
	plan.EdgeRequestID = edgeRequestID
	plan.LeaseID = leaseID
	plan.LeaseTTLMS = int(ttl / time.Millisecond)
	plan.LowLatencyMode = h.openAIEdgeLowLatencyMode()
	h.storeOpenAIEdgeLease(&openAIEdgeLease{
		edgeRequestID:      edgeRequestID,
		leaseID:            leaseID,
		createdAt:          time.Now(),
		expiresAt:          time.Now().Add(ttl),
		userReleaseFunc:    userReleaseFunc,
		accountReleaseFunc: accountReleaseFunc,
		apiKey:             apiKey,
		subject:            subject,
		subscription:       subscription,
		account:            account,
		requestBody:        append([]byte(nil), req.Body...),
		forwardBody:        append([]byte(nil), forwardBody...),
		sessionHash:        sessionHash,
		failedAccountIDs:   make(map[int64]struct{}),
		sameAccountRetries: make(map[int64]int),
		maxAccountSwitches: h.nonImageStreamBootstrapSwitchLimit(true),
		routingModel:       reqModel,
		requestModel:       prepared.Model,
		billingModel:       prepared.BillingModel,
		upstreamModel:      prepared.UpstreamModel,
		reasoningEffort:    prepared.ReasoningEffort,
		serviceTier:        prepared.ServiceTier,
		userAgent:          c.GetHeader("User-Agent"),
		clientIP:           openAIEdgeClientIP(c, req),
		inboundEndpoint:    "/v1/chat/completions",
		upstreamEndpoint:   service.OpenAIEdgeRawChatUpstreamEndpoint(account),
		requestPayloadHash: service.HashUsageRequestPayload(req.Body),
		channelUsageFields: channelMapping.ToUsageFields(reqModel, prepared.UpstreamModel),
	}, ttl)
	releaseUserOnFailure = false
	releaseAccountOnFailure = false
	return plan, true
}

func (h *OpenAIGatewayHandler) prepareOpenAIEdgeRawResponsesRelay(c *gin.Context, req service.OpenAIEdgePrepareRequest, cfg config.GatewayOpenAIEdgeRSConfig) (service.OpenAIEdgePlan, bool) {
	fallback := func(reason string) (service.OpenAIEdgePlan, bool) {
		c.JSON(http.StatusOK, openAIEdgeFallbackPlan(req, reason, cfg, h.openAIEdgeLowLatencyMode()))
		return service.OpenAIEdgePlan{}, false
	}
	if normalizeOpenAIEdgePath(req.Path) != "/v1/responses" {
		return fallback("edge_route_not_supported")
	}
	if openAIEdgeHeader(req.Headers, "Upgrade") != "" {
		return h.prepareOpenAIEdgeResponsesWSRelay(c, req, cfg)
	}
	if h == nil || h.gatewayService == nil || h.billingCacheService == nil || h.apiKeyService == nil || h.concurrencyHelper == nil {
		return fallback("edge_dependencies_missing")
	}
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodPost) {
		return fallback("edge_route_not_supported")
	}
	if req.Stream == nil || !*req.Stream || !gjson.GetBytes(req.Body, "stream").Bool() {
		return fallback("edge_only_stream_responses_supported")
	}
	if len(req.Body) == 0 || !gjson.ValidBytes(req.Body) {
		return fallback("invalid_json_body")
	}
	reqModel := strings.TrimSpace(gjson.GetBytes(req.Body, "model").String())
	if reqModel == "" {
		return fallback("missing_model")
	}
	if strings.TrimSpace(gjson.GetBytes(req.Body, "previous_response_id").String()) != "" {
		return fallback("previous_response_id_requires_go")
	}
	if bytes.Contains(req.Body, []byte("function_call_output")) {
		return fallback("function_call_output_requires_go")
	}
	if service.IsImageGenerationIntent("/v1/responses", reqModel, req.Body) {
		return fallback("image_responses_require_go")
	}
	applyOpenAIEdgeClientHeaders(c, req.Headers)
	apiKey, subject, subscription, reason := h.authenticateOpenAIEdgeClient(c, req)
	if reason != "" {
		return fallback(reason)
	}
	if !cfg.RelayResponses {
		return fallback("edge_responses_relay_disabled")
	}
	if reason := openAIEdgeRolloutRejectReason(cfg, apiKey, reqModel, req.EdgeRequestID); reason != "" {
		return fallback(reason)
	}
	reqLog := requestLogger(c, "handler.openai_edge.prepare_responses",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("model", reqModel),
	)
	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, req.Body); decision != nil && decision.Blocked {
		return fallback("content_moderation_blocked")
	}
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardBody := []byte(req.Body)
	if channelMapping.Mapped {
		forwardBody = h.gatewayService.ReplaceModelInBody(forwardBody, channelMapping.MappedModel)
	}

	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(c.Request.Context(), subject.UserID, subject.Concurrency)
	if err != nil {
		reqLog.Warn("openai_edge.responses_user_slot_acquire_failed", zap.Error(err))
		return fallback("user_slot_acquire_failed")
	}
	if !userAcquired {
		return fallback("user_slot_busy")
	}
	releaseUserOnFailure := true
	defer func() {
		if releaseUserOnFailure && userReleaseFunc != nil {
			userReleaseFunc()
		}
	}()
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_edge.responses_billing_check_failed", zap.Error(err))
		return fallback("billing_check_failed")
	}
	sessionHash := h.gatewayService.GeneratePromptCacheBoostAffinitySessionHash(c, req.Body, reqModel)
	if sessionHash == "" {
		sessionHash = h.gatewayService.GenerateSessionHash(c, req.Body)
	}
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
		c.Request.Context(),
		apiKey.GroupID,
		"",
		sessionHash,
		reqModel,
		nil,
		service.OpenAIUpstreamTransportAny,
		service.OpenAIEndpointCapabilityChatCompletions,
		false,
	)
	if err != nil || selection == nil || selection.Account == nil {
		if err != nil {
			reqLog.Warn("openai_edge.responses_account_select_failed", zap.Error(err))
		}
		return fallback("account_select_failed")
	}
	account := selection.Account
	sessionHash = h.gatewayService.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, account)
	sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
	if account.Type != service.AccountTypeOAuth && !service.IsOpenAIEdgeRawResponsesRelayEligible(account) {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return fallback("account_requires_go_responses_transform")
	}
	accountReleaseFunc := selection.ReleaseFunc
	if !selection.Acquired {
		if selection.WaitPlan == nil {
			return fallback("account_slot_not_available")
		}
		var accountAcquired bool
		accountReleaseFunc, accountAcquired, err = h.concurrencyHelper.TryAcquireAccountSlot(c.Request.Context(), account.ID, selection.WaitPlan.MaxConcurrency)
		if err != nil {
			reqLog.Warn("openai_edge.responses_account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			return fallback("account_slot_acquire_failed")
		}
		if !accountAcquired {
			return fallback("account_slot_busy")
		}
		if err := h.gatewayService.BindStickySession(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai_edge.responses_bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
	}
	releaseAccountOnFailure := true
	defer func() {
		if releaseAccountOnFailure && accountReleaseFunc != nil {
			accountReleaseFunc()
		}
	}()
	var prepared *service.OpenAIEdgePreparedChatCompletions
	if account.Type == service.AccountTypeOAuth {
		prepared, err = h.gatewayService.BuildChatGPTOAuthResponsesEdgePlan(c.Request.Context(), c, account, forwardBody)
	} else {
		prepared, err = h.gatewayService.BuildRawResponsesEdgePlan(c.Request.Context(), c, account, forwardBody)
	}
	if err != nil {
		reqLog.Warn("openai_edge.build_responses_plan_failed", zap.Int64("account_id", account.ID), zap.String("account_type", string(account.Type)), zap.Error(err))
		return fallback("build_responses_plan_failed")
	}
	leaseID := "lease_" + uuid.NewString()
	edgeRequestID := strings.TrimSpace(req.EdgeRequestID)
	if edgeRequestID == "" {
		edgeRequestID = "edge_" + uuid.NewString()
	}
	ttl := time.Duration(cfg.LeaseTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	plan := prepared.Plan
	plan.EdgeRequestID = edgeRequestID
	plan.LeaseID = leaseID
	plan.LeaseTTLMS = int(ttl / time.Millisecond)
	plan.LowLatencyMode = h.openAIEdgeLowLatencyMode()
	h.storeOpenAIEdgeLease(&openAIEdgeLease{
		edgeRequestID:      edgeRequestID,
		leaseID:            leaseID,
		createdAt:          time.Now(),
		expiresAt:          time.Now().Add(ttl),
		userReleaseFunc:    userReleaseFunc,
		accountReleaseFunc: accountReleaseFunc,
		apiKey:             apiKey,
		subject:            subject,
		subscription:       subscription,
		account:            account,
		requestBody:        append([]byte(nil), req.Body...),
		forwardBody:        append([]byte(nil), forwardBody...),
		sessionHash:        sessionHash,
		failedAccountIDs:   make(map[int64]struct{}),
		sameAccountRetries: make(map[int64]int),
		maxAccountSwitches: h.nonImageStreamBootstrapSwitchLimit(true),
		routingModel:       reqModel,
		requestModel:       prepared.Model,
		billingModel:       prepared.BillingModel,
		upstreamModel:      prepared.UpstreamModel,
		reasoningEffort:    prepared.ReasoningEffort,
		serviceTier:        prepared.ServiceTier,
		userAgent:          c.GetHeader("User-Agent"),
		clientIP:           openAIEdgeClientIP(c, req),
		inboundEndpoint:    "/v1/responses",
		upstreamEndpoint:   "/v1/responses",
		requestPayloadHash: service.HashUsageRequestPayload(req.Body),
		channelUsageFields: channelMapping.ToUsageFields(reqModel, prepared.UpstreamModel),
	}, ttl)
	releaseUserOnFailure = false
	releaseAccountOnFailure = false
	return plan, true
}

func (h *OpenAIGatewayHandler) prepareOpenAIEdgeResponsesWSRelay(c *gin.Context, req service.OpenAIEdgePrepareRequest, cfg config.GatewayOpenAIEdgeRSConfig) (service.OpenAIEdgePlan, bool) {
	fallback := func(reason string) (service.OpenAIEdgePlan, bool) {
		c.JSON(http.StatusOK, openAIEdgeFallbackPlan(req, reason, cfg, h.openAIEdgeLowLatencyMode()))
		return service.OpenAIEdgePlan{}, false
	}
	if !cfg.RelayResponsesWebSocket {
		return fallback("edge_responses_ws_relay_disabled")
	}
	if h == nil || h.gatewayService == nil || h.billingCacheService == nil || h.apiKeyService == nil || h.concurrencyHelper == nil {
		return fallback("edge_dependencies_missing")
	}
	if !strings.EqualFold(openAIEdgeHeader(req.Headers, "Upgrade"), "websocket") {
		return fallback("edge_ws_upgrade_required")
	}
	if len(req.Body) == 0 || !gjson.ValidBytes(req.Body) {
		return fallback("invalid_json_body")
	}
	reqModel := strings.TrimSpace(gjson.GetBytes(req.Body, "model").String())
	if reqModel == "" {
		return fallback("missing_model")
	}
	if strings.TrimSpace(gjson.GetBytes(req.Body, "previous_response_id").String()) != "" {
		return fallback("previous_response_id_requires_go_ws")
	}
	if bytes.Contains(req.Body, []byte("function_call_output")) {
		return fallback("function_call_output_requires_go_ws")
	}
	if service.IsImageGenerationIntent("/v1/responses", reqModel, req.Body) {
		return fallback("image_ws_requires_go")
	}
	applyOpenAIEdgeClientHeaders(c, req.Headers)
	apiKey, subject, subscription, reason := h.authenticateOpenAIEdgeClient(c, req)
	if reason != "" {
		return fallback(reason)
	}
	if reason := openAIEdgeRolloutRejectReason(cfg, apiKey, reqModel, req.EdgeRequestID); reason != "" {
		return fallback(reason)
	}
	reqLog := requestLogger(c, "handler.openai_edge.prepare_responses_ws",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("model", reqModel),
	)
	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, req.Body); decision != nil && decision.Blocked {
		return fallback("content_moderation_blocked")
	}
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardBody := []byte(req.Body)
	if channelMapping.Mapped {
		forwardBody = h.gatewayService.ReplaceModelInBody(forwardBody, channelMapping.MappedModel)
	}
	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlot(c.Request.Context(), subject.UserID, subject.Concurrency)
	if err != nil {
		reqLog.Warn("openai_edge.responses_ws_user_slot_acquire_failed", zap.Error(err))
		return fallback("user_slot_acquire_failed")
	}
	if !userAcquired {
		return fallback("user_slot_busy")
	}
	releaseUserOnFailure := true
	defer func() {
		if releaseUserOnFailure && userReleaseFunc != nil {
			userReleaseFunc()
		}
	}()
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_edge.responses_ws_billing_check_failed", zap.Error(err))
		return fallback("billing_check_failed")
	}
	sessionHash := h.gatewayService.GenerateSessionHashWithFallback(c, req.Body, openAIWSIngressFallbackSessionSeed(subject.UserID, apiKey.ID, apiKey.GroupID))
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
		c.Request.Context(),
		apiKey.GroupID,
		"",
		sessionHash,
		reqModel,
		nil,
		service.OpenAIUpstreamTransportResponsesWebsocketV2,
		service.OpenAIEndpointCapabilityChatCompletions,
		false,
	)
	if err != nil || selection == nil || selection.Account == nil {
		if err != nil {
			reqLog.Warn("openai_edge.responses_ws_account_select_failed", zap.Error(err))
		}
		return fallback("account_select_failed")
	}
	account := selection.Account
	accountReleaseFunc := selection.ReleaseFunc
	if !selection.Acquired {
		if selection.WaitPlan == nil {
			return fallback("account_slot_not_available")
		}
		var accountAcquired bool
		accountReleaseFunc, accountAcquired, err = h.concurrencyHelper.TryAcquireAccountSlot(c.Request.Context(), account.ID, selection.WaitPlan.MaxConcurrency)
		if err != nil {
			reqLog.Warn("openai_edge.responses_ws_account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			return fallback("account_slot_acquire_failed")
		}
		if !accountAcquired {
			return fallback("account_slot_busy")
		}
	}
	releaseAccountOnFailure := true
	defer func() {
		if releaseAccountOnFailure && accountReleaseFunc != nil {
			accountReleaseFunc()
		}
	}()
	if err := h.gatewayService.BindStickySession(c.Request.Context(), apiKey.GroupID, sessionHash, account.ID); err != nil {
		reqLog.Warn("openai_edge.responses_ws_bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
	}
	token, _, err := h.gatewayService.GetAccessToken(c.Request.Context(), account)
	if err != nil {
		reqLog.Warn("openai_edge.responses_ws_get_access_token_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return fallback("get_access_token_failed")
	}
	prepared, err := h.gatewayService.BuildResponsesWSEdgePlan(c.Request.Context(), c, account, forwardBody, token)
	if err != nil {
		reqLog.Warn("openai_edge.build_responses_ws_plan_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return fallback("build_responses_ws_plan_failed")
	}
	leaseID := "lease_" + uuid.NewString()
	edgeRequestID := strings.TrimSpace(req.EdgeRequestID)
	if edgeRequestID == "" {
		edgeRequestID = "edge_" + uuid.NewString()
	}
	ttl := time.Duration(cfg.LeaseTTLMS) * time.Millisecond
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	plan := prepared.Plan
	plan.EdgeRequestID = edgeRequestID
	plan.LeaseID = leaseID
	plan.LeaseTTLMS = int(ttl / time.Millisecond)
	plan.LowLatencyMode = h.openAIEdgeLowLatencyMode()
	h.storeOpenAIEdgeLease(&openAIEdgeLease{
		edgeRequestID:      edgeRequestID,
		leaseID:            leaseID,
		createdAt:          time.Now(),
		expiresAt:          time.Now().Add(ttl),
		userReleaseFunc:    userReleaseFunc,
		accountReleaseFunc: accountReleaseFunc,
		apiKey:             apiKey,
		subject:            subject,
		subscription:       subscription,
		account:            account,
		requestBody:        append([]byte(nil), req.Body...),
		forwardBody:        append([]byte(nil), forwardBody...),
		sessionHash:        sessionHash,
		failedAccountIDs:   make(map[int64]struct{}),
		sameAccountRetries: make(map[int64]int),
		maxAccountSwitches: h.maxAccountSwitches,
		routingModel:       reqModel,
		requestModel:       prepared.Model,
		billingModel:       prepared.BillingModel,
		upstreamModel:      prepared.UpstreamModel,
		reasoningEffort:    prepared.ReasoningEffort,
		serviceTier:        prepared.ServiceTier,
		userAgent:          c.GetHeader("User-Agent"),
		clientIP:           openAIEdgeClientIP(c, req),
		inboundEndpoint:    "/v1/responses:ws",
		upstreamEndpoint:   "wss:/v1/responses",
		requestPayloadHash: service.HashUsageRequestPayload(req.Body),
		channelUsageFields: channelMapping.ToUsageFields(reqModel, prepared.UpstreamModel),
	}, ttl)
	releaseUserOnFailure = false
	releaseAccountOnFailure = false
	return plan, true
}

func openAIEdgeRolloutRejectReason(cfg config.GatewayOpenAIEdgeRSConfig, apiKey *service.APIKey, requestedModel string, edgeRequestID string) string {
	if apiKey == nil {
		return "api_key_missing"
	}
	if len(cfg.AllowedAPIKeyIDs) > 0 && !int64InList(apiKey.ID, cfg.AllowedAPIKeyIDs) {
		return "edge_api_key_not_in_rollout"
	}
	if len(cfg.AllowedGroupIDs) > 0 {
		if apiKey.GroupID == nil || !int64InList(*apiKey.GroupID, cfg.AllowedGroupIDs) {
			return "edge_group_not_in_rollout"
		}
	}
	if len(cfg.AllowedModels) > 0 && !stringInListFold(requestedModel, cfg.AllowedModels) {
		return "edge_model_not_in_rollout"
	}
	if cfg.RolloutPercent <= 0 {
		return "edge_rollout_percent_zero"
	}
	if cfg.RolloutPercent >= 100 {
		return ""
	}
	seed := strings.TrimSpace(edgeRequestID)
	if seed == "" {
		seed = strconv.FormatInt(apiKey.ID, 10) + ":" + strings.TrimSpace(requestedModel)
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(seed))
	if int(hasher.Sum32()%100) >= cfg.RolloutPercent {
		return "edge_rollout_percent_skip"
	}
	return ""
}

func int64InList(v int64, list []int64) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func stringInListFold(v string, list []string) bool {
	v = strings.TrimSpace(v)
	for _, item := range list {
		if strings.EqualFold(v, strings.TrimSpace(item)) {
			return true
		}
	}
	return false
}

func normalizeOpenAIEdgePath(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "/openai/v1/") {
		return "/v1/" + strings.TrimPrefix(path, "/openai/v1/")
	}
	return path
}

func applyOpenAIEdgeClientHeaders(c *gin.Context, headers map[string]string) {
	if c == nil || c.Request == nil {
		return
	}
	for k, v := range headers {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		c.Request.Header.Set(k, v)
	}
}

func openAIEdgeHeader(headers map[string]string, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func normalizeOpenAIEdgePrepareBody(req *service.OpenAIEdgePrepareRequest) error {
	if req == nil || strings.TrimSpace(req.BodyRawBase64) == "" {
		return nil
	}
	body, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.BodyRawBase64))
	if err != nil {
		return err
	}
	req.Body = append(req.Body[:0], body...)
	return nil
}

func openAIEdgeClientIP(c *gin.Context, req service.OpenAIEdgePrepareRequest) string {
	if clientIP := strings.TrimSpace(req.ClientIP); clientIP != "" {
		return clientIP
	}
	return ip.GetClientIP(c)
}

func openAIEdgeAPIKeyFromHeaders(headers map[string]string) string {
	authHeader := openAIEdgeHeader(headers, "Authorization")
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	if key := openAIEdgeHeader(headers, "x-api-key"); key != "" {
		return key
	}
	return openAIEdgeHeader(headers, "x-goog-api-key")
}

func (h *OpenAIGatewayHandler) authenticateOpenAIEdgeClient(c *gin.Context, req service.OpenAIEdgePrepareRequest) (*service.APIKey, middleware2.AuthSubject, *service.UserSubscription, string) {
	if h == nil || h.apiKeyService == nil {
		return nil, middleware2.AuthSubject{}, nil, "api_key_service_missing"
	}
	apiKeyString := openAIEdgeAPIKeyFromHeaders(req.Headers)
	if apiKeyString == "" {
		return nil, middleware2.AuthSubject{}, nil, "api_key_missing"
	}
	apiKey, err := h.apiKeyService.GetByKey(c.Request.Context(), apiKeyString)
	if err != nil {
		if errors.Is(err, service.ErrAPIKeyNotFound) {
			return nil, middleware2.AuthSubject{}, nil, "api_key_invalid"
		}
		return nil, middleware2.AuthSubject{}, nil, "api_key_lookup_failed"
	}
	if apiKey == nil || apiKey.User == nil {
		return nil, middleware2.AuthSubject{}, nil, "api_key_user_missing"
	}
	if !apiKey.IsActive() || apiKey.IsExpired() || apiKey.IsQuotaExhausted() {
		return nil, middleware2.AuthSubject{}, nil, "api_key_not_billable"
	}
	if !apiKey.User.IsActive() {
		return nil, middleware2.AuthSubject{}, nil, "user_inactive"
	}
	if len(apiKey.IPWhitelist) > 0 || len(apiKey.IPBlacklist) > 0 {
		return nil, middleware2.AuthSubject{}, nil, "ip_acl_requires_go_auth"
	}
	if apiKey.Group != nil {
		if !apiKey.Group.IsActive() || !service.IsGroupContextValid(apiKey.Group) {
			return nil, middleware2.AuthSubject{}, nil, "group_not_available"
		}
		ctx := context.WithValue(c.Request.Context(), ctxkey.Group, apiKey.Group)
		c.Request = c.Request.WithContext(ctx)
	}
	var subscription *service.UserSubscription
	if apiKey.Group != nil && apiKey.Group.IsSubscriptionType() {
		sub, subErr := h.apiKeyService.GetActiveSubscriptionForAPIKey(c.Request.Context(), apiKey)
		if subErr != nil || sub == nil {
			return nil, middleware2.AuthSubject{}, nil, "subscription_requires_go_auth"
		}
		subscription = sub
	}
	_ = h.apiKeyService.TouchLastUsed(c.Request.Context(), apiKey.ID)
	subject := middleware2.AuthSubject{UserID: apiKey.User.ID, Concurrency: apiKey.User.Concurrency}
	c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware2.ContextKeyUser), subject)
	if subscription != nil {
		c.Set(string(middleware2.ContextKeySubscription), subscription)
	}
	return apiKey, subject, subscription, ""
}

func (h *OpenAIGatewayHandler) OpenAIEdgeRetry(c *gin.Context) {
	if !h.requireOpenAIEdgeSecret(c) {
		return
	}
	var req service.OpenAIEdgeRetryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid retry request"})
		return
	}
	decision := h.openAIEdgeRetryDecision(c, req)
	c.JSON(http.StatusOK, decision)
}

func (h *OpenAIGatewayHandler) openAIEdgeRetryDecision(c *gin.Context, req service.OpenAIEdgeRetryRequest) service.OpenAIEdgeRetryDecision {
	fallback := func(reason string) service.OpenAIEdgeRetryDecision {
		return service.OpenAIEdgeRetryDecision{Action: service.OpenAIEdgeActionFallbackGo, Reason: reason}
	}
	if req.WroteClientResponse {
		return fallback("client_response_already_written")
	}
	lease := h.getOpenAIEdgeLease(req.LeaseID)
	if lease == nil || lease.account == nil {
		return fallback("lease_not_found")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if req.AccountID != 0 && req.AccountID != lease.account.ID {
		return fallback("account_mismatch")
	}
	status := req.UpstreamStatusCode
	if !service.OpenAIEdgeHTTPStatusRetryable(status) {
		return fallback("upstream_status_not_retryable")
	}
	if h == nil || h.gatewayService == nil || h.concurrencyHelper == nil {
		return fallback("edge_dependencies_missing")
	}
	reqLog := requestLogger(c, "handler.openai_edge.retry",
		zap.String("lease_id", lease.leaseID),
		zap.Int64("account_id", lease.account.ID),
		zap.Int("upstream_status", status),
	)
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:             status,
		ResponseBody:           openAIEdgeRetryResponseBody(req),
		Message:                strings.TrimSpace(req.ErrorMessage),
		RetryableOnSameAccount: lease.account.IsPoolMode() && lease.account.IsPoolModeRetryableStatus(status),
	}
	if failoverErr.RetryableOnSameAccount {
		retryLimit := lease.account.GetPoolModeRetryCount()
		if lease.sameAccountRetries[lease.account.ID] < retryLimit {
			lease.sameAccountRetries[lease.account.ID]++
			plan, err := h.buildOpenAIEdgeRetryPlan(c, lease, lease.account, lease.accountReleaseFunc)
			if err != nil {
				reqLog.Warn("openai_edge.same_account_retry_plan_failed", zap.Error(err))
				return fallback("same_account_retry_plan_failed")
			}
			reqLog.Warn("openai_edge.same_account_retry",
				zap.Int("retry_limit", retryLimit),
				zap.Int("retry_count", lease.sameAccountRetries[lease.account.ID]),
			)
			return service.OpenAIEdgeRetryDecision{Action: service.OpenAIEdgeActionRelay, Reason: "same_account_retry", Plan: &plan}
		}
	}
	routingModel := lease.openAIRoutingModel()
	h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, routingModel, false, nil)
	h.gatewayService.HandleOpenAIAccountFailoverSwitch(c.Request.Context(), lease.apiKey.GroupID, lease.sessionHash, lease.account, failoverErr, routingModel)
	h.gatewayService.RecordOpenAIAccountSwitch()
	lease.failedAccountIDs[lease.account.ID] = struct{}{}
	if lease.switchCount >= lease.maxAccountSwitches {
		return fallback("max_account_switches_exhausted")
	}
	lease.switchCount++
	if h.gatewayService.ShouldStopOpenAIOAuth429Failover(lease.account, status, lease.switchCount) {
		return fallback("oauth_429_storm_stop")
	}
	accountReleaseFuncToCall := lease.accountReleaseFunc
	lease.accountReleaseFunc = nil
	if accountReleaseFuncToCall != nil {
		accountReleaseFuncToCall()
	}
	selection, _, err := h.gatewayService.SelectAccountWithSchedulerForCapability(
		c.Request.Context(),
		lease.apiKey.GroupID,
		"",
		lease.sessionHash,
		routingModel,
		lease.failedAccountIDs,
		service.OpenAIUpstreamTransportAny,
		service.OpenAIEndpointCapabilityChatCompletions,
		false,
	)
	if err != nil || selection == nil || selection.Account == nil {
		if err != nil {
			reqLog.Warn("openai_edge.retry_account_select_failed", zap.Error(err))
		}
		return fallback("retry_account_select_failed")
	}
	account := selection.Account
	if !service.IsOpenAIEdgeRawRelayEligibleForInboundEndpoint(account, lease.inboundEndpoint) {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return fallback("retry_account_requires_go_transform")
	}
	accountReleaseFunc := selection.ReleaseFunc
	if !selection.Acquired {
		if selection.WaitPlan == nil {
			return fallback("retry_account_slot_not_available")
		}
		var acquired bool
		accountReleaseFunc, acquired, err = h.concurrencyHelper.TryAcquireAccountSlot(c.Request.Context(), account.ID, selection.WaitPlan.MaxConcurrency)
		if err != nil {
			reqLog.Warn("openai_edge.retry_account_slot_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			return fallback("retry_account_slot_failed")
		}
		if !acquired {
			return fallback("retry_account_slot_busy")
		}
		if err := h.gatewayService.BindStickySession(c.Request.Context(), lease.apiKey.GroupID, lease.sessionHash, account.ID); err != nil {
			reqLog.Warn("openai_edge.retry_bind_sticky_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
	}
	plan, err := h.buildOpenAIEdgeRetryPlan(c, lease, account, accountReleaseFunc)
	if err != nil {
		if accountReleaseFunc != nil {
			accountReleaseFunc()
		}
		reqLog.Warn("openai_edge.retry_plan_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		return fallback("retry_plan_failed")
	}
	lease.account = account
	lease.accountReleaseFunc = accountReleaseFunc
	return service.OpenAIEdgeRetryDecision{Action: service.OpenAIEdgeActionRelay, Reason: "account_switch", Plan: &plan}
}

func (h *OpenAIGatewayHandler) buildOpenAIEdgeRetryPlan(c *gin.Context, lease *openAIEdgeLease, account *service.Account, release func()) (service.OpenAIEdgePlan, error) {
	var (
		prepared *service.OpenAIEdgePreparedChatCompletions
		err      error
	)
	if lease.inboundEndpoint == "/v1/responses" {
		prepared, err = h.gatewayService.BuildRawResponsesEdgePlan(c.Request.Context(), c, account, lease.forwardBody)
	} else {
		prepared, err = h.gatewayService.BuildRawChatCompletionsEdgePlan(c.Request.Context(), c, account, lease.forwardBody, "")
	}
	if err != nil {
		return service.OpenAIEdgePlan{}, err
	}
	plan := prepared.Plan
	plan.EdgeRequestID = lease.edgeRequestID
	plan.LeaseID = lease.leaseID
	plan.LeaseTTLMS = int(time.Until(lease.expiresAt) / time.Millisecond)
	if plan.LeaseTTLMS <= 0 {
		plan.LeaseTTLMS = 1
	}
	plan.LowLatencyMode = h.openAIEdgeLowLatencyMode()
	lease.account = account
	lease.accountReleaseFunc = release
	lease.requestModel = prepared.Model
	lease.billingModel = prepared.BillingModel
	lease.upstreamModel = prepared.UpstreamModel
	lease.reasoningEffort = prepared.ReasoningEffort
	lease.serviceTier = prepared.ServiceTier
	lease.upstreamEndpoint = service.OpenAIEdgeRawUpstreamEndpointForInbound(account, lease.inboundEndpoint)
	return plan, nil
}

func openAIEdgeRetryResponseBody(req service.OpenAIEdgeRetryRequest) []byte {
	if len(req.ResponseBody) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(req.ResponseBody, &text); err == nil {
		return []byte(text)
	}
	return append([]byte(nil), req.ResponseBody...)
}

func (h *OpenAIGatewayHandler) OpenAIEdgeComplete(c *gin.Context) {
	if !h.requireOpenAIEdgeSecret(c) {
		return
	}
	var req service.OpenAIEdgeCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid complete request"})
		return
	}
	lease := h.takeOpenAIEdgeLease(req.LeaseID)
	if lease == nil {
		c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true, Reason: "lease_not_found_or_already_released"})
		return
	}
	lease.release()
	if req.AccountID != 0 && lease.account != nil && req.AccountID != lease.account.ID {
		if h.gatewayService != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, lease.openAIRoutingModel(), false, nil)
		}
		c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true, Reason: "account_mismatch"})
		return
	}
	if req.Success && h.gatewayService != nil && lease.account != nil {
		firstTokenMs := intPointerFromInt64(req.FirstTokenMS)
		h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, lease.openAIRoutingModel(), true, firstTokenMs)
		result := &service.OpenAIForwardResult{
			RequestID:       req.RequestID,
			ResponseID:      req.ResponseID,
			Usage:           req.Usage,
			Model:           lease.requestModel,
			BillingModel:    lease.billingModel,
			UpstreamModel:   lease.upstreamModel,
			ServiceTier:     lease.serviceTier,
			ReasoningEffort: lease.reasoningEffort,
			Stream:          true,
			Duration:        time.Duration(req.DurationMS) * time.Millisecond,
			FirstTokenMs:    firstTokenMs,
		}
		h.submitOpenAIUsageRecordTask(context.Background(), result, func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:             result,
				APIKey:             lease.apiKey,
				User:               lease.apiKey.User,
				Account:            lease.account,
				Subscription:       lease.subscription,
				InboundEndpoint:    lease.inboundEndpoint,
				UpstreamEndpoint:   lease.upstreamEndpoint,
				UserAgent:          lease.userAgent,
				IPAddress:          lease.clientIP,
				RequestPayloadHash: lease.requestPayloadHash,
				APIKeyService:      h.apiKeyService,
				ChannelUsageFields: lease.channelUsageFields,
			}); err != nil {
				requestLogger(c, "handler.openai_edge.complete").Error("openai_edge.record_usage_failed",
					zap.Int64("account_id", lease.account.ID),
					zap.Error(err),
				)
			}
		})
	} else if h.gatewayService != nil && lease.account != nil {
		h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, lease.openAIRoutingModel(), false, nil)
	}
	c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true})
}

func (h *OpenAIGatewayHandler) OpenAIEdgeAbort(c *gin.Context) {
	if !h.requireOpenAIEdgeSecret(c) {
		return
	}
	var req service.OpenAIEdgeAbortRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid abort request"})
		return
	}
	lease := h.takeOpenAIEdgeLease(req.LeaseID)
	if lease != nil {
		lease.release()
		if h.gatewayService != nil && lease.account != nil {
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, lease.openAIRoutingModel(), false, nil)
		}
	}
	c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true})
}

func intPointerFromInt64(v *int64) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}
