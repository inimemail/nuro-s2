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
	settled            bool
	edgeRequestID      string
	edgeNodeID         string
	edgeInstanceID     string
	leaseID            string
	createdAt          time.Time
	expiresAt          time.Time
	releaseOnce        sync.Once
	userReleaseFunc    func()
	accountReleaseFunc func()
	apiKey             *service.APIKey
	subject            middleware2.AuthSubject
	subscription       *service.UserSubscription
	quotaPlatform      string
	account            *service.Account
	cachePolicyEnabled bool
	cachePolicyApplied bool
	forwardBody        []byte
	sessionHash        string
	failedAccountIDs   map[int64]struct{}
	sameAccountRetries map[int64]int
	sameAccountStarted map[int64]time.Time
	switchCount        int
	maxAccountSwitches int
	lockedPriority     int
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

type openAIEdgeAccountSelection struct {
	account         *service.Account
	releaseFunc     func()
	userReleaseFunc func()
}

const (
	openAIEdgePrepareCacheMaxEntries = 8192
	openAIEdgeCancelledMaxEntries    = 8192
	openAIEdgeCancelledCleanupEvery  = time.Minute
)

type openAIEdgePrepareCache struct {
	mu             sync.Mutex
	ttl            time.Duration
	maxEntries     int
	channelMapping map[string]openAIEdgeCachedChannelMapping
}

type openAIEdgeCachedChannelMapping struct {
	mapping   service.ChannelMappingResult
	expiresAt time.Time
}

func newOpenAIEdgePrepareCache(ttl time.Duration, maxEntries int) *openAIEdgePrepareCache {
	if ttl <= 0 || maxEntries <= 0 {
		return nil
	}
	return &openAIEdgePrepareCache{
		ttl:            ttl,
		maxEntries:     maxEntries,
		channelMapping: make(map[string]openAIEdgeCachedChannelMapping),
	}
}

func (c *openAIEdgePrepareCache) getChannelMapping(key string, now time.Time) (service.ChannelMappingResult, bool) {
	if c == nil || strings.TrimSpace(key) == "" {
		return service.ChannelMappingResult{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.channelMapping[key]
	if !ok || now.After(entry.expiresAt) {
		delete(c.channelMapping, key)
		return service.ChannelMappingResult{}, false
	}
	return entry.mapping, true
}

func (c *openAIEdgePrepareCache) setChannelMapping(key string, mapping service.ChannelMappingResult, now time.Time) {
	if c == nil || strings.TrimSpace(key) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.channelMapping) >= c.maxEntries {
		c.channelMapping = make(map[string]openAIEdgeCachedChannelMapping)
	}
	c.channelMapping[key] = openAIEdgeCachedChannelMapping{mapping: mapping, expiresAt: now.Add(c.ttl)}
}

func openAIEdgeChannelMappingCacheKey(groupID *int64, model string) string {
	model = strings.TrimSpace(model)
	if groupID == nil || model == "" {
		return ""
	}
	return strconv.FormatInt(*groupID, 10) + ":" + model
}

func (h *OpenAIGatewayHandler) resolveOpenAIEdgeChannelMapping(ctx context.Context, groupID *int64, model string) service.ChannelMappingResult {
	if h == nil || h.gatewayService == nil {
		return service.ChannelMappingResult{MappedModel: model}
	}
	now := time.Now()
	cacheKey := openAIEdgeChannelMappingCacheKey(groupID, model)
	if cached, ok := h.openAIEdgePrepareCache.getChannelMapping(cacheKey, now); ok {
		return cached
	}
	mapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(ctx, groupID, model)
	h.openAIEdgePrepareCache.setChannelMapping(cacheKey, mapping, now)
	return mapping
}

func (h *OpenAIGatewayHandler) selectOpenAIEdgeAccountWithSlot(
	ctx context.Context,
	groupID *int64,
	userID int64,
	userConcurrency int,
	apiKeyID int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	baseExcluded map[int64]struct{},
	requiredTransport service.OpenAIUpstreamTransport,
	requiredCapability service.OpenAIEndpointCapability,
	eligible func(*service.Account) bool,
	bindSticky bool,
	reqLog *zap.Logger,
) (*openAIEdgeAccountSelection, string, error) {
	capacitySkippedIDs := make(map[int64]struct{})
	localSkippedIDs := make(map[int64]struct{})
	var userReleaseFunc func()
	userSlotHeld := false
	defer func() {
		if userReleaseFunc != nil {
			userReleaseFunc()
		}
	}()
	for {
		excludedIDs := mergeOpenAIAccountExclusions(baseExcluded, capacitySkippedIDs, localSkippedIDs)
		var (
			selection *service.AccountSelectionResult
			err       error
		)
		if !userSlotHeld && userID > 0 && userConcurrency > 0 {
			selection, _, err = h.gatewayService.SelectAccountWithSchedulerForCapabilityAndUserSlot(
				ctx,
				groupID,
				userID,
				userConcurrency,
				previousResponseID,
				sessionHash,
				requestedModel,
				excludedIDs,
				requiredTransport,
				requiredCapability,
				false,
			)
			if err == nil && selection != nil && selection.UserReleaseFunc != nil {
				userReleaseFunc = h.concurrencyHelper.withAPIKeySlot(ctx, apiKeyID, selection.UserReleaseFunc)
				userSlotHeld = true
			}
			if err != nil || selection == nil || selection.Account == nil {
				if userReleaseFunc != nil {
					userReleaseFunc()
					userReleaseFunc = nil
					userSlotHeld = false
				}
				var acquired bool
				userReleaseFunc, acquired, err = h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, userID, userConcurrency, apiKeyID)
				if err != nil {
					return nil, "user_slot_acquire_failed", err
				}
				if !acquired {
					return nil, "user_slot_busy", nil
				}
				userSlotHeld = true
				selection = nil
				err = nil
			}
		}
		if selection == nil || selection.Account == nil {
			selection, _, err = h.gatewayService.SelectAccountWithSchedulerForCapability(
				ctx,
				groupID,
				previousResponseID,
				sessionHash,
				requestedModel,
				excludedIDs,
				requiredTransport,
				requiredCapability,
				false,
			)
		}
		if err != nil {
			return nil, "account_select_failed", err
		}
		if selection == nil || selection.Account == nil {
			switch {
			case len(localSkippedIDs) > 0:
				return nil, "edge_no_eligible_account", nil
			case len(capacitySkippedIDs) > 0:
				return nil, "edge_account_capacity_exhausted", nil
			default:
				return nil, "account_select_failed", nil
			}
		}
		account := selection.Account
		if eligible != nil && !eligible(account) {
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if selection.UserReleaseFunc != nil {
				if userReleaseFunc != nil {
					userReleaseFunc()
				} else {
					selection.UserReleaseFunc()
				}
				userReleaseFunc = nil
				userSlotHeld = false
			}
			localSkippedIDs[account.ID] = struct{}{}
			if reqLog != nil {
				reqLog.Info("openai_edge.account_local_skip",
					zap.Int64("account_id", account.ID),
					zap.String("reason", "edge_ineligible"),
					zap.Int("local_skipped_count", len(localSkippedIDs)),
				)
			}
			continue
		}
		accountReleaseFunc := selection.ReleaseFunc
		if selection.UserReleaseFunc != nil && userReleaseFunc == nil {
			userReleaseFunc = h.concurrencyHelper.withAPIKeySlot(ctx, apiKeyID, selection.UserReleaseFunc)
			userSlotHeld = true
		}
		if !selection.Acquired {
			if selection.WaitPlan == nil {
				capacitySkippedIDs[account.ID] = struct{}{}
				continue
			}
			var acquired bool
			accountReleaseFunc, acquired, err = h.concurrencyHelper.TryAcquireAccountSlot(ctx, account.ID, selection.WaitPlan.MaxConcurrency)
			if err != nil {
				return nil, "account_slot_acquire_failed", err
			}
			if !acquired {
				capacitySkippedIDs[account.ID] = struct{}{}
				if reqLog != nil {
					reqLog.Info("openai_edge.account_capacity_skip",
						zap.Int64("account_id", account.ID),
						zap.String("reason", "account_slot_busy"),
						zap.Int("capacity_skipped_count", len(capacitySkippedIDs)),
					)
				}
				continue
			}
			if bindSticky {
				if err := h.gatewayService.BindStickySession(ctx, groupID, sessionHash, account.ID); err != nil && reqLog != nil {
					reqLog.Warn("openai_edge.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
				}
			}
		}
		if !userSlotHeld && userID > 0 && userConcurrency > 0 {
			var acquired bool
			userReleaseFunc, acquired, err = h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, userID, userConcurrency, apiKeyID)
			if err != nil {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
				return nil, "user_slot_acquire_failed", err
			}
			if !acquired {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
				return nil, "user_slot_busy", nil
			}
			userSlotHeld = true
		}
		resultUserReleaseFunc := userReleaseFunc
		userReleaseFunc = nil
		return &openAIEdgeAccountSelection{account: account, releaseFunc: accountReleaseFunc, userReleaseFunc: resultUserReleaseFunc}, "", nil
	}
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

func openAIEdgeLaneFromServiceTier(serviceTier *string) string {
	serviceTierValue := ""
	if serviceTier != nil {
		serviceTierValue = *serviceTier
	}
	switch strings.ToLower(strings.TrimSpace(serviceTierValue)) {
	case "priority":
		return "priority"
	default:
		return "normal"
	}
}

func openAIEdgeHasToolOutput(body []byte) bool {
	if !bytes.Contains(body, []byte("function_call_output")) &&
		!bytes.Contains(body, []byte("tool_search_output")) &&
		!bytes.Contains(body, []byte("custom_tool_call_output")) &&
		!bytes.Contains(body, []byte("mcp_tool_call_output")) {
		return false
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return true
	}
	return service.ValidateFunctionCallOutputContext(reqBody).HasFunctionCallOutput
}

func openAIEdgeToolOutputFallbackReason(body []byte) string {
	if !openAIEdgeHasToolOutput(body) {
		return ""
	}
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return "function_call_output_requires_go"
	}
	validation := service.ValidateFunctionCallOutputContext(reqBody)
	if !validation.HasFunctionCallOutput {
		return ""
	}
	if validation.HasToolCallContext || validation.HasItemReferenceForAllCallIDs {
		return ""
	}
	if validation.HasFunctionCallOutputMissingCallID {
		return "function_call_output_missing_call_id_requires_go"
	}
	return "function_call_output_requires_go"
}

func (h *OpenAIGatewayHandler) openAIEdgeLowLatencyMode() string {
	if h == nil || h.cfg == nil {
		return ""
	}
	return h.cfg.StreamLowLatencyMode()
}

func (h *OpenAIGatewayHandler) cleanupOpenAIEdgeCancelledLocked(now time.Time) {
	if h == nil {
		return
	}
	if len(h.openAIEdgeCancelled) < openAIEdgeCancelledMaxEntries &&
		!h.openAIEdgeCancelledNext.IsZero() && now.Before(h.openAIEdgeCancelledNext) {
		return
	}
	for edgeRequestID, expiresAt := range h.openAIEdgeCancelled {
		if !expiresAt.After(now) {
			delete(h.openAIEdgeCancelled, edgeRequestID)
		}
	}
	h.openAIEdgeCancelledNext = now.Add(openAIEdgeCancelledCleanupEvery)
	if len(h.openAIEdgeCancelled) < openAIEdgeCancelledMaxEntries {
		return
	}
	var oldestID string
	var oldestExpiry time.Time
	for edgeRequestID, expiresAt := range h.openAIEdgeCancelled {
		if oldestID == "" || expiresAt.Before(oldestExpiry) {
			oldestID = edgeRequestID
			oldestExpiry = expiresAt
		}
	}
	if oldestID != "" {
		delete(h.openAIEdgeCancelled, oldestID)
	}
}

func (h *OpenAIGatewayHandler) markOpenAIEdgeCancelledLocked(edgeRequestID string, ttl time.Duration) {
	edgeRequestID = strings.TrimSpace(edgeRequestID)
	if h == nil || edgeRequestID == "" {
		return
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	now := time.Now()
	if h.openAIEdgeCancelled == nil {
		h.openAIEdgeCancelled = make(map[string]time.Time)
	}
	h.cleanupOpenAIEdgeCancelledLocked(now)
	h.openAIEdgeCancelled[edgeRequestID] = now.Add(ttl)
}

func (h *OpenAIGatewayHandler) storeOpenAIEdgeLease(lease *openAIEdgeLease, ttl time.Duration) bool {
	if h == nil || lease == nil || lease.leaseID == "" {
		return false
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	edgeRequestID := strings.TrimSpace(lease.edgeRequestID)
	h.openAIEdgeLeaseMu.Lock()
	now := time.Now()
	h.cleanupOpenAIEdgeCancelledLocked(now)
	if expiresAt, cancelled := h.openAIEdgeCancelled[edgeRequestID]; cancelled && expiresAt.After(now) {
		h.openAIEdgeLeaseMu.Unlock()
		return false
	}
	if h.openAIEdgeLeases == nil {
		h.openAIEdgeLeases = make(map[string]*openAIEdgeLease)
	}
	if h.openAIEdgeLeaseByRequest == nil {
		h.openAIEdgeLeaseByRequest = make(map[string]string)
	}
	if edgeRequestID != "" && h.openAIEdgeLeaseByRequest[edgeRequestID] != "" {
		h.openAIEdgeLeaseMu.Unlock()
		return false
	}
	h.openAIEdgeLeases[lease.leaseID] = lease
	if edgeRequestID != "" {
		h.openAIEdgeLeaseByRequest[edgeRequestID] = lease.leaseID
	}
	h.openAIEdgeLeaseMu.Unlock()
	timer := time.AfterFunc(ttl, func() {
		expired := false
		h.openAIEdgeLeaseMu.Lock()
		current := h.openAIEdgeLeases[lease.leaseID]
		if current == lease {
			lease.mu.Lock()
			if !lease.settled {
				lease.settled = true
				delete(h.openAIEdgeLeases, lease.leaseID)
				if h.openAIEdgeLeaseByRequest[edgeRequestID] == lease.leaseID {
					delete(h.openAIEdgeLeaseByRequest, edgeRequestID)
				}
				expired = true
			}
			lease.mu.Unlock()
		}
		h.openAIEdgeLeaseMu.Unlock()
		if expired {
			lease.release()
		}
	})
	lease.mu.Lock()
	if lease.settled {
		lease.mu.Unlock()
		timer.Stop()
		return true
	}
	lease.timer = timer
	lease.mu.Unlock()
	return true
}

func (h *OpenAIGatewayHandler) takeOpenAIEdgeLeaseForRequest(leaseID, edgeRequestID string, accountID int64, verifyAccount bool) (*openAIEdgeLease, string) {
	if h == nil {
		return nil, ""
	}
	leaseID = strings.TrimSpace(leaseID)
	edgeRequestID = strings.TrimSpace(edgeRequestID)
	h.openAIEdgeLeaseMu.Lock()
	defer h.openAIEdgeLeaseMu.Unlock()
	if leaseID != "" && edgeRequestID != "" {
		if currentLeaseID := h.openAIEdgeLeaseByRequest[edgeRequestID]; currentLeaseID != "" && currentLeaseID != leaseID {
			return nil, "lease_mismatch"
		}
	}
	if leaseID == "" && edgeRequestID != "" {
		leaseID = h.openAIEdgeLeaseByRequest[edgeRequestID]
	}
	if leaseID == "" {
		return nil, ""
	}
	lease := h.openAIEdgeLeases[leaseID]
	if lease == nil {
		return nil, ""
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if edgeRequestID != "" && edgeRequestID != lease.edgeRequestID {
		return nil, "request_mismatch"
	}
	if verifyAccount && accountID != 0 && (lease.account == nil || lease.account.ID != accountID) {
		return nil, "account_mismatch"
	}
	delete(h.openAIEdgeLeases, leaseID)
	if h.openAIEdgeLeaseByRequest[lease.edgeRequestID] == leaseID {
		delete(h.openAIEdgeLeaseByRequest, lease.edgeRequestID)
	}
	lease.settled = true
	return lease, ""
}

func (h *OpenAIGatewayHandler) cancelOpenAIEdgeLeaseForRequest(leaseID, edgeRequestID string, _ int64, ttl time.Duration) (*openAIEdgeLease, string) {
	if h == nil {
		return nil, ""
	}
	leaseID = strings.TrimSpace(leaseID)
	edgeRequestID = strings.TrimSpace(edgeRequestID)
	h.openAIEdgeLeaseMu.Lock()
	defer h.openAIEdgeLeaseMu.Unlock()
	if leaseID != "" && edgeRequestID != "" {
		if currentLeaseID := h.openAIEdgeLeaseByRequest[edgeRequestID]; currentLeaseID != "" && currentLeaseID != leaseID {
			return nil, "lease_mismatch"
		}
	}
	if leaseID == "" && edgeRequestID != "" {
		leaseID = h.openAIEdgeLeaseByRequest[edgeRequestID]
	}
	lease := h.openAIEdgeLeases[leaseID]
	if lease != nil {
		lease.mu.Lock()
		defer lease.mu.Unlock()
		if edgeRequestID != "" && edgeRequestID != lease.edgeRequestID {
			return nil, "request_mismatch"
		}
		delete(h.openAIEdgeLeases, leaseID)
		if h.openAIEdgeLeaseByRequest[lease.edgeRequestID] == leaseID {
			delete(h.openAIEdgeLeaseByRequest, lease.edgeRequestID)
		}
		lease.settled = true
		if edgeRequestID == "" {
			edgeRequestID = lease.edgeRequestID
		}
	}
	h.markOpenAIEdgeCancelledLocked(edgeRequestID, ttl)
	return lease, ""
}

func (h *OpenAIGatewayHandler) getOpenAIEdgeLease(leaseID string) *openAIEdgeLease {
	if h == nil || strings.TrimSpace(leaseID) == "" {
		return nil
	}
	h.openAIEdgeLeaseMu.Lock()
	defer h.openAIEdgeLeaseMu.Unlock()
	return h.openAIEdgeLeases[leaseID]
}

func (h *OpenAIGatewayHandler) recoverOpenAIEdgeLeases(edgeNodeID, currentInstanceID string) int {
	if h == nil {
		return 0
	}
	edgeNodeID = strings.TrimSpace(edgeNodeID)
	currentInstanceID = strings.TrimSpace(currentInstanceID)
	if edgeNodeID == "" || currentInstanceID == "" {
		return 0
	}
	var stale []*openAIEdgeLease
	h.openAIEdgeLeaseMu.Lock()
	for leaseID, lease := range h.openAIEdgeLeases {
		if lease == nil || strings.TrimSpace(lease.edgeNodeID) != edgeNodeID ||
			strings.TrimSpace(lease.edgeInstanceID) == "" || lease.edgeInstanceID == currentInstanceID {
			continue
		}
		lease.mu.Lock()
		if !lease.settled {
			lease.settled = true
			delete(h.openAIEdgeLeases, leaseID)
			if h.openAIEdgeLeaseByRequest[lease.edgeRequestID] == leaseID {
				delete(h.openAIEdgeLeaseByRequest, lease.edgeRequestID)
			}
			h.markOpenAIEdgeCancelledLocked(lease.edgeRequestID, time.Until(lease.expiresAt))
			stale = append(stale, lease)
		}
		lease.mu.Unlock()
	}
	h.openAIEdgeLeaseMu.Unlock()
	for _, lease := range stale {
		lease.release()
	}
	return len(stale)
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
	channelMapping := h.resolveOpenAIEdgeChannelMapping(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardBody := []byte(req.Body)
	if channelMapping.Mapped {
		forwardBody = h.gatewayService.ReplaceModelInBody(forwardBody, channelMapping.MappedModel)
	}
	var userReleaseFunc func()
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
	affinityModel := reqModel
	if channelMapping.Mapped {
		affinityModel = channelMapping.MappedModel
	}
	sessionHash := h.gatewayService.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(c.Request.Context(), c, apiKey.GroupID, req.Body, reqModel, forwardBody, affinityModel)
	if sessionHash == "" {
		sessionHash = h.gatewayService.GenerateSessionHash(c, req.Body)
	}
	edgeSelection, reason, err := h.selectOpenAIEdgeAccountWithSlot(
		c.Request.Context(),
		apiKey.GroupID,
		subject.UserID,
		subject.Concurrency,
		apiKey.ID,
		"",
		sessionHash,
		reqModel,
		nil,
		service.OpenAIUpstreamTransportAny,
		service.OpenAIEndpointCapabilityChatCompletions,
		service.IsOpenAIEdgeRawChatRelayEligible,
		true,
		reqLog,
	)
	if err != nil || edgeSelection == nil || edgeSelection.account == nil {
		if err != nil {
			reqLog.Warn("openai_edge.account_select_failed", zap.Error(err))
		}
		if reason == "" {
			reason = "account_select_failed"
		}
		return fallback(reason)
	}
	account := edgeSelection.account
	if edgeSelection.userReleaseFunc != nil {
		if userReleaseFunc != nil {
			userReleaseFunc()
		}
		userReleaseFunc = edgeSelection.userReleaseFunc
	}
	sessionHash = h.gatewayService.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, account)
	sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
	accountReleaseFunc := edgeSelection.releaseFunc
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
	plan.Lane = openAIEdgeLaneFromServiceTier(prepared.ServiceTier)
	createdAt := time.Now()
	lease := &openAIEdgeLease{
		edgeRequestID:      edgeRequestID,
		edgeNodeID:         strings.TrimSpace(req.EdgeNodeID),
		edgeInstanceID:     strings.TrimSpace(req.EdgeInstanceID),
		leaseID:            leaseID,
		createdAt:          createdAt,
		expiresAt:          createdAt.Add(ttl),
		userReleaseFunc:    userReleaseFunc,
		accountReleaseFunc: accountReleaseFunc,
		apiKey:             apiKey,
		subject:            subject,
		subscription:       subscription,
		quotaPlatform:      service.QuotaPlatform(c.Request.Context(), apiKey),
		account:            account,
		cachePolicyEnabled: prepared.Plan.PromptCacheCreationOptimizationMode != "",
		cachePolicyApplied: prepared.Plan.PromptCacheCreationOptimizationApplied,
		forwardBody:        append([]byte(nil), forwardBody...),
		sessionHash:        sessionHash,
		failedAccountIDs:   make(map[int64]struct{}),
		sameAccountRetries: make(map[int64]int),
		sameAccountStarted: map[int64]time.Time{account.ID: createdAt},
		maxAccountSwitches: h.nonImageStreamBootstrapSwitchLimit(true),
		lockedPriority:     -1,
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
	}
	if !h.storeOpenAIEdgeLease(lease, ttl) {
		return fallback("edge_prepare_cancelled")
	}
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
		// WS is multi-turn and must reacquire concurrency and settle usage for
		// every response. Keep it on the Go relay until edge-rs has a per-turn
		// control-plane contract.
		return fallback("edge_ws_requires_go_per_turn_governance")
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
	if reason := openAIEdgeToolOutputFallbackReason(req.Body); reason != "" {
		return fallback(reason)
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
	channelMapping := h.resolveOpenAIEdgeChannelMapping(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardBody := []byte(req.Body)
	if channelMapping.Mapped {
		forwardBody = h.gatewayService.ReplaceModelInBody(forwardBody, channelMapping.MappedModel)
	}

	var userReleaseFunc func()
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
	affinityModel := reqModel
	if channelMapping.Mapped {
		affinityModel = channelMapping.MappedModel
	}
	sessionHash := h.gatewayService.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(c.Request.Context(), c, apiKey.GroupID, req.Body, reqModel, forwardBody, affinityModel)
	if sessionHash == "" {
		sessionHash = h.gatewayService.GenerateSessionHash(c, req.Body)
	}
	edgeSelection, reason, err := h.selectOpenAIEdgeAccountWithSlot(
		c.Request.Context(),
		apiKey.GroupID,
		subject.UserID,
		subject.Concurrency,
		apiKey.ID,
		"",
		sessionHash,
		reqModel,
		nil,
		service.OpenAIUpstreamTransportAny,
		service.OpenAIEndpointCapabilityChatCompletions,
		func(account *service.Account) bool {
			return account != nil && (account.Type == service.AccountTypeOAuth || service.IsOpenAIEdgeRawResponsesRelayEligible(account))
		},
		true,
		reqLog,
	)
	if err != nil || edgeSelection == nil || edgeSelection.account == nil {
		if err != nil {
			reqLog.Warn("openai_edge.responses_account_select_failed", zap.Error(err))
		}
		if reason == "" {
			reason = "account_select_failed"
		}
		return fallback(reason)
	}
	account := edgeSelection.account
	if edgeSelection.userReleaseFunc != nil {
		if userReleaseFunc != nil {
			userReleaseFunc()
		}
		userReleaseFunc = edgeSelection.userReleaseFunc
	}
	sessionHash = h.gatewayService.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, account)
	sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
	accountReleaseFunc := edgeSelection.releaseFunc
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
	plan.Lane = openAIEdgeLaneFromServiceTier(prepared.ServiceTier)
	createdAt := time.Now()
	lease := &openAIEdgeLease{
		edgeRequestID:      edgeRequestID,
		edgeNodeID:         strings.TrimSpace(req.EdgeNodeID),
		edgeInstanceID:     strings.TrimSpace(req.EdgeInstanceID),
		leaseID:            leaseID,
		createdAt:          createdAt,
		expiresAt:          createdAt.Add(ttl),
		userReleaseFunc:    userReleaseFunc,
		accountReleaseFunc: accountReleaseFunc,
		apiKey:             apiKey,
		subject:            subject,
		subscription:       subscription,
		quotaPlatform:      service.QuotaPlatform(c.Request.Context(), apiKey),
		account:            account,
		cachePolicyEnabled: prepared.Plan.PromptCacheCreationOptimizationMode != "",
		cachePolicyApplied: prepared.Plan.PromptCacheCreationOptimizationApplied,
		forwardBody:        append([]byte(nil), forwardBody...),
		sessionHash:        sessionHash,
		failedAccountIDs:   make(map[int64]struct{}),
		sameAccountRetries: make(map[int64]int),
		sameAccountStarted: map[int64]time.Time{account.ID: createdAt},
		maxAccountSwitches: h.nonImageStreamBootstrapSwitchLimit(true),
		lockedPriority:     -1,
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
	}
	if !h.storeOpenAIEdgeLease(lease, ttl) {
		return fallback("edge_prepare_cancelled")
	}
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
	channelMapping := h.resolveOpenAIEdgeChannelMapping(c.Request.Context(), apiKey.GroupID, reqModel)
	forwardBody := []byte(req.Body)
	if channelMapping.Mapped {
		forwardBody = h.gatewayService.ReplaceModelInBody(forwardBody, channelMapping.MappedModel)
	}
	var userReleaseFunc func()
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
	edgeSelection, reason, err := h.selectOpenAIEdgeAccountWithSlot(
		c.Request.Context(),
		apiKey.GroupID,
		subject.UserID,
		subject.Concurrency,
		apiKey.ID,
		"",
		sessionHash,
		reqModel,
		nil,
		service.OpenAIUpstreamTransportResponsesWebsocketV2,
		service.OpenAIEndpointCapabilityChatCompletions,
		nil,
		false,
		reqLog,
	)
	if err != nil || edgeSelection == nil || edgeSelection.account == nil {
		if err != nil {
			reqLog.Warn("openai_edge.responses_ws_account_select_failed", zap.Error(err))
		}
		if reason == "" {
			reason = "account_select_failed"
		}
		return fallback(reason)
	}
	account := edgeSelection.account
	if edgeSelection.userReleaseFunc != nil {
		if userReleaseFunc != nil {
			userReleaseFunc()
		}
		userReleaseFunc = edgeSelection.userReleaseFunc
	}
	accountReleaseFunc := edgeSelection.releaseFunc
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
	plan.Lane = openAIEdgeLaneFromServiceTier(prepared.ServiceTier)
	createdAt := time.Now()
	lease := &openAIEdgeLease{
		edgeRequestID:      edgeRequestID,
		edgeNodeID:         strings.TrimSpace(req.EdgeNodeID),
		edgeInstanceID:     strings.TrimSpace(req.EdgeInstanceID),
		leaseID:            leaseID,
		createdAt:          createdAt,
		expiresAt:          createdAt.Add(ttl),
		userReleaseFunc:    userReleaseFunc,
		accountReleaseFunc: accountReleaseFunc,
		apiKey:             apiKey,
		subject:            subject,
		subscription:       subscription,
		quotaPlatform:      service.QuotaPlatform(c.Request.Context(), apiKey),
		account:            account,
		cachePolicyEnabled: prepared.Plan.PromptCacheCreationOptimizationMode != "",
		cachePolicyApplied: prepared.Plan.PromptCacheCreationOptimizationApplied,
		forwardBody:        append([]byte(nil), forwardBody...),
		sessionHash:        sessionHash,
		failedAccountIDs:   make(map[int64]struct{}),
		sameAccountRetries: make(map[int64]int),
		sameAccountStarted: map[int64]time.Time{account.ID: createdAt},
		maxAccountSwitches: h.maxAccountSwitches,
		lockedPriority:     -1,
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
	}
	if !h.storeOpenAIEdgeLease(lease, ttl) {
		return fallback("edge_prepare_cancelled")
	}
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
	if lease == nil {
		return fallback("lease_not_found")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.settled || lease.account == nil {
		return fallback("lease_already_settled")
	}
	if req.AccountID != 0 && req.AccountID != lease.account.ID {
		return fallback("account_mismatch")
	}
	if strings.TrimSpace(req.ErrorType) == "edge_queue_wait_timeout" {
		return h.openAIEdgeRetrySwitchAccount(c, lease, req, "queue_wait_timeout")
	}
	status := req.UpstreamStatusCode
	responseBody := openAIEdgeRetryResponseBody(req)
	upstreamMsg := strings.TrimSpace(req.ErrorMessage)
	if lease.cachePolicyApplied &&
		service.IsOpenAIPromptCacheCreationOptimizationUnsupportedError(status, upstreamMsg, responseBody) {
		if h == nil || h.gatewayService == nil {
			return fallback("cache_creation_optimization_retry_dependencies_missing")
		}
		fallbackAccount := service.OpenAIPromptCacheCreationOptimizationFallbackAccount(lease.account)
		plan, err := h.buildOpenAIEdgeRetryPlan(c, lease, fallbackAccount, lease.accountReleaseFunc)
		if err != nil {
			return fallback("cache_creation_optimization_retry_plan_failed")
		}
		return service.OpenAIEdgeRetryDecision{
			Action: service.OpenAIEdgeActionRelay,
			Reason: "cache_creation_optimization_unsupported",
			Plan:   &plan,
		}
	}
	modelRoutingError := h.openAIEdgeShouldProtectModelRoutingError(c, lease.account, status, upstreamMsg, responseBody)
	if !service.OpenAIEdgeHTTPStatusRetryable(status) && !modelRoutingError {
		return fallback("upstream_status_not_retryable")
	}
	if h == nil || h.gatewayService == nil || h.concurrencyHelper == nil {
		if modelRoutingError {
			return openAIEdgeModelRoutingErrorDecision("model_routing_error_protected")
		}
		return fallback("edge_dependencies_missing")
	}
	reqLog := requestLogger(c, "handler.openai_edge.retry",
		zap.String("lease_id", lease.leaseID),
		zap.Int64("account_id", lease.account.ID),
		zap.Int("upstream_status", status),
	)
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:             status,
		ResponseBody:           responseBody,
		Message:                upstreamMsg,
		RetryableOnSameAccount: service.OpenAIPoolFailoverRetryableOnSameAccount(lease.account, status, upstreamMsg, responseBody),
		SkipPoolSoftCooldown:   modelRoutingError,
	}
	if failoverErr.RetryableOnSameAccount {
		// Edge retries are intentionally immediate. The upstream attempt and the
		// Rust -> Go control-plane round trip already consume the elapsed budget;
		// adding the account delay here would directly increase TTFT.
		if retryPlan, ok := planSameAccountRetry(lease.account, lease.sameAccountRetries, lease.sameAccountStarted, 0); ok {
			plan, err := h.buildOpenAIEdgeRetryPlan(c, lease, lease.account, lease.accountReleaseFunc)
			if err != nil {
				reqLog.Warn("openai_edge.same_account_retry_plan_failed", zap.Error(err))
				return fallback("same_account_retry_plan_failed")
			}
			reqLog.Warn("openai_edge.same_account_retry",
				zap.Int("retry_limit", retryPlan.RetryLimit),
				zap.Int("retry_count", retryPlan.RetryCount),
				zap.Duration("retry_elapsed", retryPlan.Elapsed),
				zap.Duration("retry_max_elapsed", retryPlan.MaxElapsed),
			)
			return service.OpenAIEdgeRetryDecision{
				Action: service.OpenAIEdgeActionRelay,
				Reason: "same_account_retry",
				Plan:   &plan,
			}
		}
	}
	routingModel := lease.openAIRoutingModel()
	h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, routingModel, false, nil)
	h.gatewayService.HandleOpenAIAccountFailoverSwitch(c.Request.Context(), lease.apiKey.GroupID, lease.sessionHash, lease.account, failoverErr, routingModel)
	h.gatewayService.RecordOpenAIAccountSwitch()
	lease.failedAccountIDs[lease.account.ID] = struct{}{}
	if lease.switchCount >= lease.maxAccountSwitches {
		if modelRoutingError {
			decision := openAIEdgeModelRoutingErrorDecision("model_routing_error_exhausted")
			decision.FailureRecorded = true
			return decision
		}
		decision := fallback("max_account_switches_exhausted")
		decision.FailureRecorded = true
		return decision
	}
	lease.switchCount++
	if h.gatewayService.ShouldStopOpenAIOAuth429Failover(lease.account, status, lease.switchCount) {
		decision := fallback("oauth_429_storm_stop")
		decision.FailureRecorded = true
		return decision
	}
	decision := h.openAIEdgeRetrySwitchAccount(c, lease, req, "account_switch")
	decision.FailureRecorded = true
	return decision
}

func (h *OpenAIGatewayHandler) openAIEdgeShouldProtectModelRoutingError(c *gin.Context, account *service.Account, status int, upstreamMsg string, responseBody []byte) bool {
	if account == nil || !account.IsPoolMode() {
		return false
	}
	if h != nil && h.gatewayService != nil && !h.gatewayService.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(c.Request.Context()) {
		return false
	}
	return service.IsOpenAIPoolModelRoutingError(status, upstreamMsg, responseBody)
}

func openAIEdgeModelRoutingErrorDecision(reason string) service.OpenAIEdgeRetryDecision {
	return service.OpenAIEdgeRetryDecision{
		Action:       service.OpenAIEdgeActionRespondError,
		Reason:       reason,
		StatusCode:   http.StatusBadRequest,
		ErrorType:    "invalid_request_error",
		ErrorMessage: service.OpenAIPoolModelRoutingClientMessage(),
	}
}

func (h *OpenAIGatewayHandler) openAIEdgeRetrySwitchAccount(c *gin.Context, lease *openAIEdgeLease, req service.OpenAIEdgeRetryRequest, successReason string) service.OpenAIEdgeRetryDecision {
	fallback := func(reason string) service.OpenAIEdgeRetryDecision {
		return service.OpenAIEdgeRetryDecision{Action: service.OpenAIEdgeActionFallbackGo, Reason: reason}
	}
	if h == nil || h.gatewayService == nil {
		return fallback("edge_dependencies_missing")
	}
	reqLog := requestLogger(c, "handler.openai_edge.retry_switch",
		zap.String("lease_id", lease.leaseID),
		zap.String("reason", successReason),
		zap.Int64("account_id", lease.account.ID),
	)
	routingModel := lease.openAIRoutingModel()
	responseBody := openAIEdgeRetryResponseBody(req)
	modelRoutingError := h.openAIEdgeShouldProtectModelRoutingError(c, lease.account, req.UpstreamStatusCode, strings.TrimSpace(req.ErrorMessage), responseBody)
	if modelRoutingError && lease.lockedPriority < 0 {
		lease.lockedPriority = lease.account.Priority
	}
	if successReason == "queue_wait_timeout" {
		// Queue wait timeout is local edge-rs pressure, not an upstream account
		// failure. Exclude this account for the immediate retry without feeding a
		// false failure sample into scheduler EWMA/soft-cooldown decisions.
		lease.failedAccountIDs[lease.account.ID] = struct{}{}
		if lease.switchCount >= lease.maxAccountSwitches {
			return fallback("max_account_switches_exhausted")
		}
		lease.switchCount++
	}
	accountReleaseFuncToCall := lease.accountReleaseFunc
	lease.accountReleaseFunc = nil
	if accountReleaseFuncToCall != nil {
		accountReleaseFuncToCall()
	}
	edgeSelection, reason, err := h.selectOpenAIEdgeAccountWithSlot(
		c.Request.Context(),
		lease.apiKey.GroupID,
		0,
		0,
		0,
		"",
		lease.sessionHash,
		routingModel,
		lease.failedAccountIDs,
		service.OpenAIUpstreamTransportAny,
		service.OpenAIEndpointCapabilityChatCompletions,
		func(account *service.Account) bool {
			return service.IsOpenAIEdgeRawRelayEligibleForInboundEndpoint(account, lease.inboundEndpoint) &&
				(lease.lockedPriority < 0 || account.Priority == lease.lockedPriority)
		},
		true,
		reqLog,
	)
	if err != nil || edgeSelection == nil || edgeSelection.account == nil {
		if err != nil {
			reqLog.Warn("openai_edge.retry_account_select_failed", zap.Error(err))
		}
		if reason == "" {
			reason = "retry_account_select_failed"
		}
		if modelRoutingError {
			return openAIEdgeModelRoutingErrorDecision("model_routing_error_no_retry_account")
		}
		return fallback("retry_" + reason)
	}
	account := edgeSelection.account
	accountReleaseFunc := edgeSelection.releaseFunc
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
	return service.OpenAIEdgeRetryDecision{Action: service.OpenAIEdgeActionRelay, Reason: successReason, Plan: &plan}
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
	plan.Lane = openAIEdgeLaneFromServiceTier(prepared.ServiceTier)
	lease.account = account
	lease.accountReleaseFunc = release
	markSameAccountAttemptStart(lease.sameAccountStarted, account, time.Now())
	lease.requestModel = prepared.Model
	lease.billingModel = prepared.BillingModel
	lease.upstreamModel = prepared.UpstreamModel
	lease.reasoningEffort = prepared.ReasoningEffort
	lease.serviceTier = prepared.ServiceTier
	lease.upstreamEndpoint = service.OpenAIEdgeRawUpstreamEndpointForInbound(account, lease.inboundEndpoint)
	lease.cachePolicyEnabled = plan.PromptCacheCreationOptimizationMode != ""
	lease.cachePolicyApplied = plan.PromptCacheCreationOptimizationApplied
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
	lease, mismatchReason := h.takeOpenAIEdgeLeaseForRequest(req.LeaseID, req.EdgeRequestID, req.AccountID, true)
	if mismatchReason != "" {
		c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true, Reason: mismatchReason})
		return
	}
	if lease == nil {
		c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true, Reason: "lease_not_found_or_already_released"})
		return
	}
	lease.release()
	if h.gatewayService != nil && lease.account != nil {
		terminalType := strings.ToLower(strings.TrimSpace(req.TerminalEventType))
		successfulTerminal := openAIEdgeCompletionIsSuccessful(lease.inboundEndpoint, req)
		neutralOutcome := req.ClientDisconnected || req.CyberBlocked || terminalType == "response.incomplete" ||
			terminalType == "response.cancelled" || terminalType == "response.canceled"
		cachePolicyCompatibilityFailure := openAIEdgeCachePolicyCompatibilityFailure(lease, req)
		firstTokenMs := intPointerFromInt64(req.FirstTokenMS)
		if !successfulTerminal {
			firstTokenMs = nil
		}
		if realFirstTokenMs := intPointerFromInt64(req.RealFirstTokenMS); successfulTerminal && realFirstTokenMs != nil {
			h.gatewayService.RecordOpenAIFirstTokenTimeoutPlaceholderGuardSample(
				lease.account,
				lease.openAIRoutingModel(),
				*realFirstTokenMs,
			)
		}
		edgeFallbackReason := stringPointerFromTrimmed(req.EdgeFallbackReason)
		result := &service.OpenAIForwardResult{
			RequestID:           req.RequestID,
			ResponseID:          req.ResponseID,
			Usage:               req.Usage,
			Model:               lease.requestModel,
			BillingModel:        lease.billingModel,
			UpstreamModel:       lease.upstreamModel,
			ServiceTier:         lease.serviceTier,
			ReasoningEffort:     lease.reasoningEffort,
			Stream:              true,
			TerminalEventType:   terminalType,
			ClientDisconnect:    req.ClientDisconnected,
			CyberBlocked:        req.CyberBlocked,
			Duration:            time.Duration(req.DurationMS) * time.Millisecond,
			FirstTokenMs:        firstTokenMs,
			UpstreamHeaderMs:    intPointerFromInt64(req.UpstreamHeaderMS),
			UpstreamFirstByteMs: intPointerFromInt64(req.UpstreamFirstByteMS),
			FirstClientFlushMs:  intPointerFromInt64(req.FirstClientFlushMS),
			EdgePrepareMs:       intPointerFromInt64(req.EdgePrepareMS),
			EdgeQueueWaitMs:     intPointerFromInt64(req.EdgeQueueWaitMS),
			EdgeRelayStartMs:    intPointerFromInt64(req.EdgeRelayStartMS),
			EdgeFallbackReason:  edgeFallbackReason,
			EdgeRetryCount:      intPointerFromInt64(req.EdgeRetryCount),
		}

		switch {
		case successfulTerminal:
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, lease.openAIRoutingModel(), true, firstTokenMs)
		case neutralOutcome || cachePolicyCompatibilityFailure:
			// Client cancellation and protocol-level incomplete/cancelled terminal
			// states are billable but are not account-health samples. Optional cache
			// policy incompatibility after an early client flush is also neutral.
		default:
			statusCode := req.UpstreamStatusCode
			if statusCode < http.StatusBadRequest {
				statusCode = http.StatusBadGateway
			}
			message := strings.TrimSpace(req.ErrorMessage)
			if message == "" {
				message = "Upstream request failed"
			}
			responseBody := []byte(message)
			h.gatewayService.HandleOpenAIAccountUpstreamErrorAfterCommittedResponse(
				c.Request.Context(), lease.account, statusCode, nil, responseBody, lease.openAIRoutingModel(),
			)
			h.gatewayService.RecordOpenAIPromptCacheBoostUnsupportedAfterCommittedResponse(
				lease.account, statusCode, message, responseBody, true, true,
			)
			h.gatewayService.RecordOpenAIPoolFailureAfterCommittedResponse(
				c.Request.Context(), lease.account, statusCode, responseBody, lease.openAIRoutingModel(), message,
			)
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, lease.openAIRoutingModel(), false, nil)
		}

		if successfulTerminal || openAIEdgeUsageIsBillable(req.Usage) || req.CyberBlocked {
			h.submitOpenAIUsageRecordTask(context.Background(), result, func(ctx context.Context) {
				if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
					Result:                  result,
					APIKey:                  lease.apiKey,
					User:                    lease.apiKey.User,
					Account:                 lease.account,
					Subscription:            lease.subscription,
					QuotaPlatform:           lease.quotaPlatform,
					InboundEndpoint:         lease.inboundEndpoint,
					UpstreamEndpoint:        lease.upstreamEndpoint,
					UserAgent:               lease.userAgent,
					IPAddress:               lease.clientIP,
					RequestPayloadHash:      lease.requestPayloadHash,
					PromptCacheAffinityHash: lease.sessionHash,
					PromptCacheGroupID:      lease.apiKey.GroupID,
					SkipSuccessSideEffects:  !successfulTerminal,
					APIKeyService:           h.apiKeyService,
					ChannelUsageFields:      lease.channelUsageFields,
					CyberBlocked:            req.CyberBlocked,
				}); err != nil {
					requestLogger(c, "handler.openai_edge.complete").Error("openai_edge.record_usage_failed",
						zap.Int64("account_id", lease.account.ID),
						zap.Error(err),
					)
				}
			})
		}
	}
	c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true})
}

func openAIEdgeSuccessfulTerminal(inboundEndpoint, terminalType string) bool {
	terminalType = strings.ToLower(strings.TrimSpace(terminalType))
	if normalizeOpenAIEdgePath(inboundEndpoint) == "/v1/chat/completions" {
		return terminalType == "[done]" || terminalType == "chat.finish_reason"
	}
	return terminalType == "response.completed" || terminalType == "response.done"
}

func openAIEdgeCompletionIsSuccessful(inboundEndpoint string, req service.OpenAIEdgeCompleteRequest) bool {
	return req.Success && !req.ClientDisconnected && !req.CyberBlocked &&
		openAIEdgeSuccessfulTerminal(inboundEndpoint, req.TerminalEventType)
}

func openAIEdgeCachePolicyCompatibilityFailure(lease *openAIEdgeLease, req service.OpenAIEdgeCompleteRequest) bool {
	return lease != nil && lease.cachePolicyEnabled &&
		strings.EqualFold(strings.TrimSpace(req.ErrorType), "cache_creation_optimization_unsupported")
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
	ttl := time.Duration(h.openAIEdgeConfig().LeaseTTLMS) * time.Millisecond
	lease, mismatchReason := h.cancelOpenAIEdgeLeaseForRequest(req.LeaseID, req.EdgeRequestID, req.AccountID, ttl)
	if mismatchReason != "" {
		c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true, Reason: mismatchReason})
		return
	}
	if lease != nil {
		lease.release()
		if h.gatewayService != nil && lease.account != nil && !req.ClientDisconnected && !openAIEdgeAbortReasonIsNeutral(req.Reason) {
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(lease.account, lease.openAIRoutingModel(), false, nil)
		}
	}
	c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true})
}

func (h *OpenAIGatewayHandler) OpenAIEdgeRecover(c *gin.Context) {
	if !h.requireOpenAIEdgeSecret(c) {
		return
	}
	var req service.OpenAIEdgeRecoverRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.EdgeNodeID) == "" || strings.TrimSpace(req.EdgeInstanceID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid recover request"})
		return
	}
	released := h.recoverOpenAIEdgeLeases(req.EdgeNodeID, req.EdgeInstanceID)
	c.JSON(http.StatusOK, service.OpenAIEdgeAck{OK: true, Released: released})
}

func openAIEdgeUsageIsBillable(usage service.OpenAIUsage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CacheCreationInputTokens > 0 ||
		usage.CacheReadInputTokens > 0 || usage.ImageOutputTokens > 0
}

func openAIEdgeAbortReasonIsNeutral(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	return strings.Contains(reason, "edge_queue_wait_timeout") ||
		strings.Contains(reason, "edge_relay_queue_full") ||
		strings.Contains(reason, "edge relay queue full") ||
		strings.Contains(reason, "queue wait budget") ||
		strings.Contains(reason, "queue_wait_timeout") ||
		strings.Contains(reason, "retry_failure_already_recorded") ||
		strings.Contains(reason, "prepare_failed") ||
		strings.Contains(reason, "ws_prepare_failed") ||
		strings.Contains(reason, "unsupported_ws_transport") ||
		strings.Contains(reason, "ws_proxy_not_supported")
}

func intPointerFromInt64(v *int64) *int {
	if v == nil {
		return nil
	}
	i := int(*v)
	return &i
}

func stringPointerFromTrimmed(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}
