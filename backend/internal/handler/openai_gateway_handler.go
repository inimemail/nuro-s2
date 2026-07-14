package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// OpenAIGatewayHandler handles OpenAI API gateway requests
type OpenAIGatewayHandler struct {
	gatewayService           *service.OpenAIGatewayService
	billingCacheService      *service.BillingCacheService
	apiKeyService            *service.APIKeyService
	usageRecordWorkerPool    *service.UsageRecordWorkerPool
	errorPassthroughService  *service.ErrorPassthroughService
	contentModerationService *service.ContentModerationService
	concurrencyHelper        *ConcurrencyHelper
	imageLimiter             *imageConcurrencyLimiter
	imageTaskRepo            service.OpenAIImageTaskRepository
	imageTaskStore           *openAIImageTaskStore
	imageTaskWorkerStop      chan struct{}
	imageTaskWorkerDone      chan struct{}
	imageTaskWorkerWG        sync.WaitGroup
	imageTaskWorkerStopOnce  sync.Once
	openAIEdgeLeaseMu        sync.Mutex
	openAIEdgeLeases         map[string]*openAIEdgeLease
	openAIEdgePrepareCache   *openAIEdgePrepareCache
	maxAccountSwitches       int
	cfg                      *config.Config
}

func resolveOpenAIMessagesDispatchMappedModel(apiKey *service.APIKey, requestedModel string) string {
	if apiKey == nil || apiKey.Group == nil {
		return ""
	}
	return strings.TrimSpace(apiKey.Group.ResolveMessagesDispatchModel(requestedModel))
}

func openAICompatibleRequestPlatform(apiKey *service.APIKey) string {
	if apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == service.PlatformGrok {
		return service.PlatformGrok
	}
	return service.PlatformOpenAI
}

type openAIModelBodyReplaceFunc func([]byte, string) []byte

type openAIAccountSlotAcquireResult struct {
	ReleaseFunc  func()
	Acquired     bool
	CapacityMiss bool
	Reason       string
	Err          error
}

func mergeOpenAIAccountExclusions(sets ...map[int64]struct{}) map[int64]struct{} {
	var merged map[int64]struct{}
	for _, set := range sets {
		for id := range set {
			if id <= 0 {
				continue
			}
			if merged == nil {
				merged = make(map[int64]struct{})
			}
			merged[id] = struct{}{}
		}
	}
	return merged
}

func openAIModelMappedBody(body []byte, mapped bool, mappedModel string, replace openAIModelBodyReplaceFunc) []byte {
	if !mapped || replace == nil {
		return body
	}
	return replace(body, mappedModel)
}

func newOpenAIModelMappedBodyCache(body []byte, replace openAIModelBodyReplaceFunc) func(bool, string) []byte {
	replacedBodies := make(map[string][]byte)
	return func(mapped bool, mappedModel string) []byte {
		if !mapped {
			return body
		}
		if cachedBody, ok := replacedBodies[mappedModel]; ok {
			return cachedBody
		}
		replacedBody := openAIModelMappedBody(body, true, mappedModel, replace)
		replacedBodies[mappedModel] = replacedBody
		return replacedBody
	}
}

func usageRecordContext(parent context.Context, base context.Context) context.Context {
	if base == nil {
		base = context.Background()
	}
	if parent == nil {
		return base
	}
	if clientRequestID, _ := parent.Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(clientRequestID) != "" {
		base = context.WithValue(base, ctxkey.ClientRequestID, strings.TrimSpace(clientRequestID))
	}
	if requestID, _ := parent.Value(ctxkey.RequestID).(string); strings.TrimSpace(requestID) != "" {
		base = context.WithValue(base, ctxkey.RequestID, strings.TrimSpace(requestID))
	}
	for _, key := range []ctxkey.Key{
		ctxkey.EdgePrepareMs,
		ctxkey.EdgeQueueWaitMs,
		ctxkey.EdgeRelayStartMs,
		ctxkey.EdgeRetryCount,
	} {
		if value, ok := parent.Value(key).(int64); ok && value >= 0 {
			base = context.WithValue(base, key, value)
		}
	}
	if reason, _ := parent.Value(ctxkey.EdgeFallbackReason).(string); strings.TrimSpace(reason) != "" {
		base = context.WithValue(base, ctxkey.EdgeFallbackReason, strings.TrimSpace(reason))
	}
	for _, item := range []struct {
		serviceKey string
		contextKey ctxkey.Key
	}{
		{service.OpsSlotWaitMsKey, ctxkey.SlotWaitMs},
		{service.OpsUpstreamHeaderMsKey, ctxkey.UpstreamHeaderMs},
		{service.OpsUpstreamFirstByteMsKey, ctxkey.UpstreamFirstByteMs},
		{service.OpsFirstClientFlushMsKey, ctxkey.FirstClientFlushMs},
	} {
		if value, ok := parent.Value(item.serviceKey).(int64); ok && value >= 0 {
			base = context.WithValue(base, item.contextKey, value)
		}
	}
	return base
}

func (h *OpenAIGatewayHandler) nonImageStreamBootstrapSwitchLimit(reqStream bool) int {
	if h == nil {
		return 0
	}
	if !reqStream || h.gatewayService == nil {
		return h.maxAccountSwitches
	}
	if retries := h.gatewayService.StreamingBootstrapRetries(); retries > 0 {
		return retries
	}
	return h.maxAccountSwitches
}

func wrapUsageRecordTaskContext(parent context.Context, task service.UsageRecordTask) service.UsageRecordTask {
	if task == nil {
		return nil
	}
	return func(ctx context.Context) {
		task(usageRecordContext(parent, ctx))
	}
}

// NewOpenAIGatewayHandler creates a new OpenAIGatewayHandler
func NewOpenAIGatewayHandler(
	gatewayService *service.OpenAIGatewayService,
	concurrencyService *service.ConcurrencyService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	errorPassthroughService *service.ErrorPassthroughService,
	contentModerationService *service.ContentModerationService,
	imageTaskRepo service.OpenAIImageTaskRepository,
	cfg *config.Config,
) *OpenAIGatewayHandler {
	pingInterval := time.Duration(0)
	maxAccountSwitches := 3
	if cfg != nil {
		pingInterval = time.Duration(cfg.Concurrency.PingInterval) * time.Second
		if cfg.Gateway.MaxAccountSwitches > 0 {
			maxAccountSwitches = cfg.Gateway.MaxAccountSwitches
		}
	}
	h := &OpenAIGatewayHandler{
		gatewayService:           gatewayService,
		billingCacheService:      billingCacheService,
		apiKeyService:            apiKeyService,
		usageRecordWorkerPool:    usageRecordWorkerPool,
		errorPassthroughService:  errorPassthroughService,
		contentModerationService: contentModerationService,
		concurrencyHelper:        NewConcurrencyHelper(concurrencyService, SSEPingFormatComment, pingInterval),
		imageLimiter:             &imageConcurrencyLimiter{},
		imageTaskRepo:            imageTaskRepo,
		imageTaskStore:           newOpenAIImageTaskStore(defaultOpenAIImageTaskRetention),
		imageTaskWorkerStop:      make(chan struct{}),
		imageTaskWorkerDone:      make(chan struct{}),
		openAIEdgePrepareCache:   newOpenAIEdgePrepareCache(2*time.Second, openAIEdgePrepareCacheMaxEntries),
		maxAccountSwitches:       maxAccountSwitches,
		cfg:                      cfg,
	}
	h.startPersistentImageTaskWorkers()
	return h
}

// Responses handles OpenAI Responses API endpoint
// POST /openai/v1/responses
func (h *OpenAIGatewayHandler) Responses(c *gin.Context) {
	// 局部兜底：确保该 handler 内部任何 panic 都不会击穿到进程级。
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)
	if h.tryOpenAIEdgeIngressProxy(c) {
		return
	}
	compactStartedAt := time.Now()
	defer h.logOpenAIRemoteCompactOutcome(c, compactStartedAt)
	setOpenAIClientTransportHTTP(c)

	requestStart := time.Now()

	// Get apiKey and user from context (set by ApiKeyAuth middleware)
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	// Read request body
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	setOpsRequestContext(c, "", false)
	sessionHashBody := body
	body, ok = h.normalizeOpenAIResponsesCompactRequest(c, reqLog, body)
	if !ok {
		return
	}
	stopCompactKeepalive := service.StartOpenAICompactSSEKeepalive(c, h.openAICompactKeepaliveInterval())
	defer stopCompactKeepalive()

	// 校验请求体 JSON 合法性
	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	// 使用 gjson 只读提取字段做校验，避免完整 Unmarshal
	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()

	reqStream, ok := parseOpenAICompatibleStream(body)
	if !ok {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", invalidStreamFieldTypeMessage)
		return
	}
	healthProbe, healthProbeErr := service.ConfigureOpenAIResponsesHealthProbe(c, body, reqModel, reqStream)
	if healthProbeErr != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", healthProbeErr.Error())
		return
	}
	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream), zap.Bool("health_probe", healthProbe))
	previousResponseID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	if previousResponseID != "" {
		previousResponseIDKind := service.ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
		reqLog = reqLog.With(
			zap.Bool("has_previous_response_id", true),
			zap.String("previous_response_id_kind", previousResponseIDKind),
			zap.Int("previous_response_id_len", len(previousResponseID)),
		)
		if previousResponseIDKind == service.OpenAIPreviousResponseIDKindMessageID {
			reqLog.Warn("openai.request_validation_failed",
				zap.String("reason", "previous_response_id_looks_like_message_id"),
			)
			h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "previous_response_id must be a response.id (resp_*), not a message id")
			return
		}
		reqLog.Warn("openai.request_validation_failed",
			zap.String("reason", "previous_response_id_requires_wsv2"),
		)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "previous_response_id is only supported on Responses WebSocket v2")
		return
	}

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, body); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}

	imageIntent := service.IsImageGenerationIntent("/v1/responses", reqModel, body)
	if imageIntent && !service.GroupAllowsImageGeneration(apiKey.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}
	requestCtx := c.Request.Context()
	if healthProbe {
		var cancelHealthProbe context.CancelFunc
		requestCtx = service.WithOpenAIHealthProbeRequestContext(requestCtx)
		requestCtx, cancelHealthProbe = context.WithTimeout(requestCtx, service.OpenAIHealthProbeTotalTimeout)
		c.Request = c.Request.WithContext(requestCtx)
		defer cancelHealthProbe()
	}
	if imageIntent {
		requestCtx = service.WithOpenAIImageGenerationIntent(requestCtx)
	}
	var imageReleaseFunc func()
	if imageIntent {
		var imageAcquired bool
		imageReleaseFunc, imageAcquired = h.acquireImageGenerationSlot(c, streamStarted)
		if !imageAcquired {
			return
		}
		if imageReleaseFunc != nil {
			defer imageReleaseFunc()
		}
	}

	// 解析渠道级模型映射
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(requestCtx, apiKey.GroupID, reqModel)
	forwardBodyForResponses := newOpenAIModelMappedBodyCache(body, h.gatewayService.ReplaceModelInBody)

	// 提前校验 function_call_output 是否具备可关联上下文，避免上游 400。
	if !h.validateFunctionCallOutputRequest(c, body, reqLog) {
		return
	}

	// 绑定错误透传服务，允许 service 层在非 failover 错误场景复用规则。
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	// Get subscription info (may be nil)
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(apiKey)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	// 2. Re-check billing eligibility after wait
	if err := h.billingCacheService.CheckBillingEligibility(requestCtx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(requestCtx, apiKey)); err != nil {
		reqLog.Info("openai.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	var sessionHash string
	if healthProbe {
		sessionHash = service.NewOpenAIHealthProbeSessionHash()
		defer h.gatewayService.ReleaseOpenAIHealthProbeSession(c.Request.Context(), apiKey.GroupID, sessionHash)
	} else {
		// Generate session hash. Header session signals win; prompt-cache affinity can
		// use either static prefixes or an explicit prompt_cache_key when enabled.
		affinityBody := forwardBodyForResponses(channelMapping.Mapped, channelMapping.MappedModel)
		affinityModel := reqModel
		if channelMapping.Mapped {
			affinityModel = channelMapping.MappedModel
		}
		sessionHash = h.gatewayService.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(requestCtx, c, apiKey.GroupID, body, reqModel, affinityBody, affinityModel)
		if sessionHash == "" {
			sessionHash = h.gatewayService.GenerateSessionHash(c, sessionHashBody)
		}
	}
	requireCompact := isOpenAIRemoteCompactPath(c)

	maxAccountSwitches := h.nonImageStreamBootstrapSwitchLimit(reqStream)
	if healthProbe {
		maxAccountSwitches = service.OpenAIHealthProbeMaxAccountSwitches
	}
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	capacitySkippedIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	sameAccountRetryStartedAt := make(map[int64]time.Time)
	var healthProbeAlternativeByAccount map[int64]bool
	if healthProbe {
		healthProbeAlternativeByAccount = make(map[int64]bool)
	}
	var lastFailoverErr *service.UpstreamFailoverError
	modelRoutingLockedPriority := -1
	userSlotHeld := false
	healthProbeDefaultFallbackStarted := false
	startHealthProbeDefaultFallback := func(failoverErr *service.UpstreamFailoverError) bool {
		if !service.ShouldStartOpenAIHealthProbeDefaultFallback(c, failoverErr, healthProbeDefaultFallbackStarted) {
			return false
		}
		fallbackBody, buildErr := service.BuildOpenAIHealthProbeDefaultFallbackBody(reqModel)
		if buildErr != nil {
			reqLog.Warn("openai.health_probe_default_fallback_build_failed", zap.Error(buildErr))
			return false
		}
		// Keep the inbound probe header/context intact; only change the body sent upstream.
		forwardBodyForResponses = newOpenAIModelMappedBodyCache(fallbackBody, h.gatewayService.ReplaceModelInBody)
		healthProbeDefaultFallbackStarted = true
		switchCount = 0
		failedAccountIDs = make(map[int64]struct{})
		capacitySkippedIDs = make(map[int64]struct{})
		sameAccountRetryCount = make(map[int64]int)
		sameAccountRetryStartedAt = make(map[int64]time.Time)
		healthProbeAlternativeByAccount = make(map[int64]bool)
		lastFailoverErr = nil
		modelRoutingLockedPriority = -1
		reqLog.Warn("openai.health_probe_default_fallback_started", zap.String("model", reqModel))
		return true
	}

	for {
		excludedAccountIDs := mergeOpenAIAccountExclusions(failedAccountIDs, capacitySkippedIDs)
		// Select account supporting the requested model
		reqLog.Debug("openai.account_selecting", zap.Int("excluded_account_count", len(excludedAccountIDs)))
		var (
			selection        *service.AccountSelectionResult
			scheduleDecision service.OpenAIAccountScheduleDecision
			err              error
		)
		if !userSlotHeld {
			selection, scheduleDecision, err = h.gatewayService.SelectAccountWithSchedulerForCapabilityAndUserSlotOnPlatform(
				requestCtx,
				apiKey.GroupID,
				subject.UserID,
				subject.Concurrency,
				previousResponseID,
				sessionHash,
				reqModel,
				excludedAccountIDs,
				service.OpenAIUpstreamTransportAny,
				service.OpenAIEndpointCapabilityChatCompletions,
				requireCompact,
				requestPlatform,
			)
			if err == nil && selection != nil && selection.UserReleaseFunc != nil {
				userSlotHeld = true
				trackedReleaseFunc := h.concurrencyHelper.withAPIKeySlot(requestCtx, apiKey.ID, selection.UserReleaseFunc)
				userReleaseFunc := wrapReleaseOnDone(requestCtx, trackedReleaseFunc)
				defer userReleaseFunc()
			}
			if err != nil || selection == nil || selection.Account == nil {
				userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
				if !acquired {
					return
				}
				if userReleaseFunc != nil {
					defer userReleaseFunc()
				}
				userSlotHeld = true
				selection = nil
				err = nil
			}
		}
		if selection == nil || selection.Account == nil {
			selection, scheduleDecision, err = h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(
				requestCtx,
				apiKey.GroupID,
				previousResponseID,
				sessionHash,
				reqModel,
				excludedAccountIDs,
				service.OpenAIUpstreamTransportAny,
				service.OpenAIEndpointCapabilityChatCompletions,
				requireCompact,
				requestPlatform,
				modelRoutingLockedPriority,
			)
		}
		if err != nil {
			reqLog.Warn("openai.account_select_failed",
				zap.Error(openAICompatibleSelectionErrorForLog(err, requestPlatform)),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				if errors.Is(err, service.ErrNoAvailableCompactAccounts) {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
					h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "compact_not_supported", "No available accounts support /responses/compact", streamStarted)
					return
				}
				cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
				return
			}
			if lastFailoverErr != nil {
				if startHealthProbeDefaultFallback(lastFailoverErr) {
					continue
				}
				h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
			} else {
				h.handleFailoverExhaustedSimple(c, 502, streamStarted)
			}
			return
		}
		if selection == nil || selection.Account == nil {
			cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
			return
		}
		if previousResponseID != "" && selection != nil && selection.Account != nil {
			reqLog.Debug("openai.account_selected_with_previous_response_id", zap.Int64("account_id", selection.Account.ID))
		}
		reqLog.Debug("openai.account_schedule_decision",
			zap.String("layer", scheduleDecision.Layer),
			zap.Bool("sticky_previous_hit", scheduleDecision.StickyPreviousHit),
			zap.Bool("sticky_session_hit", scheduleDecision.StickySessionHit),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
			zap.Int("top_k", scheduleDecision.TopK),
			zap.Int64("latency_ms", scheduleDecision.LatencyMs),
			zap.Float64("load_skew", scheduleDecision.LoadSkew),
		)
		account := selection.Account
		sessionHash = h.gatewayService.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, account)
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		setOpsSelectedAccount(c, account.ID, account.Platform)

		slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !slotResult.Acquired {
			if slotResult.CapacityMiss && previousResponseID == "" {
				capacitySkippedIDs[account.ID] = struct{}{}
				reqLog.Info("openai.account_capacity_skip",
					zap.Int64("account_id", account.ID),
					zap.String("reason", slotResult.Reason),
					zap.Int("capacity_skipped_count", len(capacitySkippedIDs)),
				)
				continue
			}
			return
		}
		accountReleaseFunc := slotResult.ReleaseFunc

		// Forward request
		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		markSameAccountAttemptStart(sameAccountRetryStartedAt, account, forwardStart)
		// 应用渠道模型映射到请求体
		forwardBody := forwardBodyForResponses(channelMapping.Mapped, channelMapping.MappedModel)
		writerSizeBeforeForward := service.OpenAICompactKeepaliveAdjustedWrittenSize(c)
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.Forward(requestCtx, c, account, forwardBody)
		}()
		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		recordSuccessfulOpenAIOpsTTFT(c, result, err)
		if err != nil {
			if shouldSettleOpenAIForwardResultAfterError(c, writerSizeBeforeForward, result) {
				reqLog.Warn("openai.forward_partial_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.String("terminal_event_type", result.TerminalEventType),
					zap.Bool("client_disconnected", result.ClientDisconnect),
					zap.Error(err),
				)
				if !result.ClientDisconnect && !openAIForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err) {
					h.ensureForwardErrorResponse(c, streamStarted)
				}
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					service.ApplyOpenAIHealthProbeRetryPolicy(c, account, failoverErr)
					service.IsolateOpenAIHealthProbeFailover(c, failoverErr)
					if service.OpenAICompactKeepaliveAdjustedWrittenSize(c) != writerSizeBeforeForward {
						h.handleFailoverExhausted(c, failoverErr, true)
						return
					}
					// 池模式：同账号重试
					if failoverErr.RetryableOnSameAccount {
						retryDelay := sameAccountRetryDelayForAccount(account)
						var retryPlan sameAccountRetryPlan
						var retry bool
						if healthProbe && account.IsOpenAIUpstreamConcurrencyRaceEnabled() {
							hasAlternative, checked := healthProbeAlternativeByAccount[account.ID]
							if !checked {
								hasAlternative = h.gatewayService.HasOpenAIHealthProbeAlternativeAccount(requestCtx, account, service.OpenAIAccountScheduleRequest{
									GroupID:            apiKey.GroupID,
									RequestedModel:     reqModel,
									RequiredTransport:  service.OpenAIUpstreamTransportAny,
									RequiredCapability: service.OpenAIEndpointCapabilityChatCompletions,
									RequireCompact:     requireCompact,
									RequestPlatform:    requestPlatform,
									ExcludedIDs:        excludedAccountIDs,
								})
								healthProbeAlternativeByAccount[account.ID] = hasAlternative
							}
							if hasAlternative {
								retryPlan, retry = planSameAccountRetryWithMaxElapsed(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay, service.OpenAIHealthProbeGrabMaxElapsed)
							} else {
								retryPlan, retry = planSameAccountRetry(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay)
							}
						} else {
							retryPlan, retry = planSameAccountRetry(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay)
						}
						if retry {
							reqLog.Warn("openai.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryPlan.RetryLimit),
								zap.Int("retry_count", retryPlan.RetryCount),
								zap.Duration("retry_delay", retryPlan.Delay),
								zap.Duration("retry_elapsed", retryPlan.Elapsed),
								zap.Duration("retry_max_elapsed", retryPlan.MaxElapsed),
							)
							select {
							case <-requestCtx.Done():
								return
							case <-time.After(retryPlan.Delay):
							}
							continue
						}
					}
					if !failoverErr.SkipSchedulePenalty {
						h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
					}
					h.gatewayService.HandleOpenAIAccountFailoverSwitch(requestCtx, apiKey.GroupID, sessionHash, account, failoverErr)
					if !healthProbe {
						h.gatewayService.RecordOpenAIAccountSwitch()
					}
					modelRoutingLockedPriority = lockOpenAIModelRoutingFailoverPriority(
						modelRoutingLockedPriority,
						account,
						failoverErr,
						h.gatewayService == nil || h.gatewayService.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(c.Request.Context()),
					)
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					if switchCount >= maxAccountSwitches {
						if startHealthProbeDefaultFallback(failoverErr) {
							continue
						}
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount) {
						h.handleFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					reqLog.Warn("openai.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", switchCount),
						zap.Int("max_switches", maxAccountSwitches),
					)
					continue
				}
				if !healthProbe && service.GetOpsCyberPolicy(c) == nil {
					h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
				}
				upstreamErrorAlreadyCommunicated := openAIForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err)
				wroteFallback := false
				if !upstreamErrorAlreadyCommunicated {
					wroteFallback = h.ensureForwardErrorResponse(c, streamStarted)
				}
				h.recordCyberPolicyUsageIfMarked(
					c.Request.Context(),
					c,
					apiKey,
					account,
					subscription,
					reqModel,
					reqStream,
					GetInboundEndpoint(c),
					GetUpstreamEndpoint(c, account.Platform),
					service.HashUsageRequestPayload(body),
					channelMapping.ToUsageFields(reqModel, ""),
				)
				fields := []zap.Field{
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Bool("upstream_error_already_communicated", upstreamErrorAlreadyCommunicated),
					zap.Error(err),
				}
				if shouldLogOpenAIForwardFailureAsWarn(c, wroteFallback) {
					reqLog.Warn("openai.forward_failed", fields...)
					return
				}
				reqLog.Error("openai.forward_failed", fields...)
				return
			}
		}
		successfulOutcome := true
		if result != nil {
			if !healthProbe && account.Type == service.AccountTypeOAuth {
				h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(c.Request.Context(), account.ID, result.ResponseHeaders)
			}
			var neutralOutcome bool
			successfulOutcome, neutralOutcome = classifyOpenAIResponsesForwardResultWithError(result, err)
			if !successfulOutcome && service.GetOpsCyberPolicy(c) != nil {
				neutralOutcome = true
			}
			if !successfulOutcome {
				result.FirstTokenMs = nil
			}
			if !healthProbe {
				switch {
				case successfulOutcome:
					h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, true, result.FirstTokenMs)
				case neutralOutcome:
					// Client disconnect and incomplete/cancelled terminal states are
					// billable when usage exists, but are not account-health samples.
				default:
					h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
				}
			}
			if !successfulOutcome && !openAIEdgeUsageIsBillable(result.Usage) && service.GetOpsCyberPolicy(c) == nil {
				reqLog.Debug("openai.request_completed_without_billable_usage",
					zap.Int64("account_id", account.ID),
					zap.String("terminal_event_type", result.TerminalEventType),
				)
				return
			}
		} else {
			if !healthProbe {
				h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, true, nil)
			}
			return
		}

		// 捕获请求信息（用于异步记录，避免在 goroutine 中访问 gin.Context）
		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
		quotaPlatform := service.QuotaPlatform(requestCtx, apiKey)

		// 使用量记录通过有界 worker 池提交，避免请求热路径创建无界 goroutine。
		cyberBlocked := result.CyberBlocked || service.GetOpsCyberPolicy(c) != nil
		h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:                  result,
				APIKey:                  apiKey,
				User:                    apiKey.User,
				Account:                 account,
				Subscription:            subscription,
				QuotaPlatform:           quotaPlatform,
				InboundEndpoint:         inboundEndpoint,
				UpstreamEndpoint:        upstreamEndpoint,
				UserAgent:               userAgent,
				IPAddress:               clientIP,
				RequestPayloadHash:      requestPayloadHash,
				PromptCacheAffinityHash: sessionHash,
				PromptCacheGroupID:      apiKey.GroupID,
				HealthProbe:             healthProbe,
				SkipSuccessSideEffects:  !successfulOutcome,
				APIKeyService:           h.apiKeyService,
				ChannelUsageFields:      channelMapping.ToUsageFields(reqModel, result.UpstreamModel),
				CyberBlocked:            cyberBlocked,
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.responses"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

func isOpenAIRemoteCompactPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	return strings.HasSuffix(normalizedPath, "/responses/compact")
}

func classifyOpenAIResponsesForwardResult(result *service.OpenAIForwardResult) (successful, neutral bool) {
	if result == nil {
		return true, false
	}
	terminalType := strings.ToLower(strings.TrimSpace(result.TerminalEventType))
	if result.ClientDisconnect {
		return false, true
	}
	if result.CyberBlocked {
		return false, true
	}
	switch terminalType {
	case "response.completed", "response.done", "[done]", "chat.finish_reason":
		return true, false
	case "response.failed", "error":
		return false, false
	case "response.incomplete", "response.cancelled", "response.canceled":
		return false, true
	default:
		return !result.Stream, false
	}
}

func classifyOpenAIResponsesForwardResultWithError(result *service.OpenAIForwardResult, forwardErr error) (successful, neutral bool) {
	successful, neutral = classifyOpenAIResponsesForwardResult(result)
	if forwardErr != nil {
		return false, neutral
	}
	return successful, neutral
}

func recordSuccessfulOpenAIOpsTTFT(c *gin.Context, result *service.OpenAIForwardResult, forwardErr error) {
	if forwardErr != nil || result == nil || result.FirstTokenMs == nil {
		return
	}
	successful, _ := classifyOpenAIResponsesForwardResult(result)
	if successful {
		service.SetOpsLatencyMs(c, service.OpsTimeToFirstTokenMsKey, int64(*result.FirstTokenMs))
	}
}

func shouldSettleOpenAIForwardResultAfterError(c *gin.Context, writerSizeBeforeForward int, result *service.OpenAIForwardResult) bool {
	if result == nil {
		return false
	}
	if result.ClientDisconnect || result.ImageCount > 0 || openAIEdgeUsageIsBillable(result.Usage) ||
		strings.TrimSpace(result.TerminalEventType) != "" {
		return true
	}
	if service.IsResponseCommitted(c) {
		return true
	}
	return service.OpenAICompactKeepaliveAdjustedWrittenSize(c) != writerSizeBeforeForward
}

func isBareOpenAIResponsesPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/")
	return strings.HasSuffix(normalizedPath, "/responses")
}

func isOpenAIRemoteCompactionV2Request(c *gin.Context, body []byte) bool {
	stream, valid := parseOpenAICompatibleStream(body)
	if !valid || !stream || c == nil || c.Request == nil {
		return false
	}
	for _, header := range c.Request.Header.Values("x-codex-beta-features") {
		for _, feature := range strings.Split(header, ",") {
			if strings.TrimSpace(feature) == "remote_compaction_v2" {
				return true
			}
		}
	}
	return false
}

func (h *OpenAIGatewayHandler) normalizeOpenAIResponsesCompactRequest(c *gin.Context, reqLog *zap.Logger, body []byte) ([]byte, bool) {
	isCompactRequest := service.IsOpenAIResponsesCompactPathForTest(c)
	if !isCompactRequest && isBareOpenAIResponsesPath(c) && service.HasCompactionTriggerInInput(body) {
		if isOpenAIRemoteCompactionV2Request(c, body) {
			return body, true
		}
		c.Request.URL.Path = strings.TrimRight(c.Request.URL.Path, "/") + "/compact"
		isCompactRequest = true
		clientStream := gjson.GetBytes(body, "stream").Bool()
		if clientStream {
			service.MarkOpenAICompactClientStream(c)
		}
		if reqLog != nil {
			reqLog.Info("codex.remote_compact.detected_body_signal", zap.Bool("client_stream", clientStream))
		}
	}
	if !isCompactRequest {
		return body, true
	}
	if compactSeed := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); compactSeed != "" {
		c.Set(service.OpenAICompactSessionSeedKeyForTest(), compactSeed)
	}
	normalizedCompactBody, normalizedCompact, compactErr := service.NormalizeOpenAICompactRequestBodyForTest(body)
	if compactErr != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to normalize compact request body")
		return nil, false
	}
	if normalizedCompact {
		body = normalizedCompactBody
	}
	return body, true
}

func (h *OpenAIGatewayHandler) logOpenAIRemoteCompactOutcome(c *gin.Context, startedAt time.Time) {
	if !isOpenAIRemoteCompactPath(c) {
		return
	}

	var (
		ctx    = context.Background()
		path   string
		status int
	)
	if c != nil {
		if c.Request != nil {
			ctx = c.Request.Context()
			if c.Request.URL != nil {
				path = strings.TrimSpace(c.Request.URL.Path)
			}
		}
		if c.Writer != nil {
			status = c.Writer.Status()
		}
	}

	outcome := "failed"
	if status >= 200 && status < 300 {
		outcome = "succeeded"
	}
	latencyMs := time.Since(startedAt).Milliseconds()
	if latencyMs < 0 {
		latencyMs = 0
	}

	fields := []zap.Field{
		zap.String("component", "handler.openai_gateway.responses"),
		zap.Bool("remote_compact", true),
		zap.String("compact_outcome", outcome),
		zap.Int("status_code", status),
		zap.Int64("latency_ms", latencyMs),
		zap.String("path", path),
		zap.Bool("force_codex_cli", h != nil && h.cfg != nil && h.cfg.Gateway.ForceCodexCLI),
	}

	if c != nil {
		if userAgent := strings.TrimSpace(c.GetHeader("User-Agent")); userAgent != "" {
			fields = append(fields, zap.String("request_user_agent", userAgent))
		}
		if v, ok := c.Get(opsModelKey); ok {
			if model, ok := v.(string); ok && strings.TrimSpace(model) != "" {
				fields = append(fields, zap.String("request_model", strings.TrimSpace(model)))
			}
		}
		if v, ok := c.Get(opsAccountIDKey); ok {
			if accountID, ok := v.(int64); ok && accountID > 0 {
				fields = append(fields, zap.Int64("account_id", accountID))
			}
		}
		if c.Writer != nil {
			if upstreamRequestID := strings.TrimSpace(c.Writer.Header().Get("x-request-id")); upstreamRequestID != "" {
				fields = append(fields, zap.String("upstream_request_id", upstreamRequestID))
			} else if upstreamRequestID := strings.TrimSpace(c.Writer.Header().Get("X-Request-Id")); upstreamRequestID != "" {
				fields = append(fields, zap.String("upstream_request_id", upstreamRequestID))
			}
		}
	}

	log := logger.FromContext(ctx).With(fields...)
	if outcome == "succeeded" {
		log.Info("codex.remote_compact.succeeded")
		return
	}
	log.Warn("codex.remote_compact.failed")
}

// Messages handles Anthropic Messages API requests routed to OpenAI platform.
// POST /v1/messages (when group platform is OpenAI)
func (h *OpenAIGatewayHandler) Messages(c *gin.Context) {
	streamStarted := false
	defer h.recoverAnthropicMessagesPanic(c, &streamStarted)

	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(
		c,
		"handler.openai_gateway.messages",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	// 检查分组是否允许 /v1/messages 调度
	if apiKey.Group != nil && !apiKey.Group.AllowMessagesDispatch {
		h.anthropicErrorResponse(c, http.StatusForbidden, "permission_error",
			"This group does not allow /v1/messages dispatch")
		return
	}

	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.anthropicErrorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return
		}
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	if len(body) == 0 {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	if !gjson.ValidBytes(body) {
		logRequestBodyParseFailure(reqLog, body, nil)
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	routingModel := service.NormalizeOpenAICompatRequestedModel(reqModel)
	preferredMappedModel := resolveOpenAIMessagesDispatchMappedModel(apiKey, reqModel)
	reqStream := gjson.GetBytes(body, "stream").Bool()

	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolAnthropicMessages, reqModel, body); decision != nil && decision.Blocked {
		h.anthropicErrorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}

	// 解析渠道级模型映射
	channelMappingMsg, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)
	mappedBodyForMessages := newOpenAIModelMappedBodyCache(body, h.gatewayService.ReplaceModelInBody)

	// 绑定错误透传服务，允许 service 层在非 failover 错误场景复用规则。
	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	requestPlatform := openAICompatibleRequestPlatform(apiKey)

	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())
	routingStart := time.Now()

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, reqStream, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_messages.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.anthropicStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	affinityBody := mappedBodyForMessages(channelMappingMsg.Mapped, channelMappingMsg.MappedModel)
	affinityModel := reqModel
	if channelMappingMsg.Mapped {
		affinityModel = channelMappingMsg.MappedModel
	}
	sessionHash := h.gatewayService.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(c.Request.Context(), c, apiKey.GroupID, body, reqModel, affinityBody, affinityModel)
	if sessionHash == "" {
		sessionHash = h.gatewayService.GenerateSessionHash(c, body)
	}
	promptCacheKey := h.gatewayService.ExtractSessionID(c, body)
	sessionHash, promptCacheKey = resolveOpenAIMessagesMetadataSession(sessionHash, promptCacheKey, reqModel, body)

	maxAccountSwitches := h.nonImageStreamBootstrapSwitchLimit(reqStream)
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	capacitySkippedIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	sameAccountRetryStartedAt := make(map[int64]time.Time)
	var lastFailoverErr *service.UpstreamFailoverError
	modelRoutingLockedPriority := -1
	effectiveMappedModel := preferredMappedModel

	for {
		excludedAccountIDs := mergeOpenAIAccountExclusions(failedAccountIDs, capacitySkippedIDs)
		currentRoutingModel := routingModel
		if effectiveMappedModel != "" {
			currentRoutingModel = effectiveMappedModel
		}
		reqLog.Debug("openai_messages.account_selecting", zap.Int("excluded_account_count", len(excludedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(
			c.Request.Context(),
			apiKey.GroupID,
			"", // no previous_response_id
			sessionHash,
			currentRoutingModel,
			excludedAccountIDs,
			service.OpenAIUpstreamTransportAny,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			requestPlatform,
			modelRoutingLockedPriority,
		)
		if err != nil {
			reqLog.Warn("openai_messages.account_select_failed",
				zap.Error(openAICompatibleSelectionErrorForLog(err, requestPlatform)),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				if err != nil {
					cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, currentRoutingModel, reqModel)
					if !cls.ModelNotFound {
						markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
					}
					h.anthropicStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
					return
				}
			} else {
				if lastFailoverErr != nil {
					h.handleAnthropicFailoverExhausted(c, lastFailoverErr, streamStarted)
				} else {
					h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
				}
				return
			}
		}
		if selection == nil || selection.Account == nil {
			cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, currentRoutingModel, reqModel)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.anthropicStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
			return
		}
		account := selection.Account
		sessionHash = h.gatewayService.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, account)
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai_messages.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		_ = scheduleDecision
		setOpsSelectedAccount(c, account.ID, account.Platform)

		slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !slotResult.Acquired {
			if slotResult.CapacityMiss {
				capacitySkippedIDs[account.ID] = struct{}{}
				reqLog.Info("openai_messages.account_capacity_skip",
					zap.Int64("account_id", account.ID),
					zap.String("reason", slotResult.Reason),
					zap.Int("capacity_skipped_count", len(capacitySkippedIDs)),
				)
				continue
			}
			return
		}
		accountReleaseFunc := slotResult.ReleaseFunc

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		markSameAccountAttemptStart(sameAccountRetryStartedAt, account, forwardStart)

		defaultMappedModel := strings.TrimSpace(effectiveMappedModel)
		// 应用渠道模型映射到请求体
		forwardBody := mappedBodyForMessages(channelMappingMsg.Mapped, channelMappingMsg.MappedModel)
		writerSizeBeforeForward := c.Writer.Size()
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.ForwardAsAnthropic(c.Request.Context(), c, account, forwardBody, promptCacheKey, defaultMappedModel)
		}()

		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)
		recordSuccessfulOpenAIOpsTTFT(c, result, err)
		if err != nil {
			if shouldSettleOpenAIForwardResultAfterError(c, writerSizeBeforeForward, result) {
				reqLog.Warn("openai_messages.forward_partial_result",
					zap.Int64("account_id", account.ID),
					zap.Int("image_count", result.ImageCount),
					zap.String("terminal_event_type", result.TerminalEventType),
					zap.Bool("client_disconnected", result.ClientDisconnect),
					zap.Error(err),
				)
				if !result.ClientDisconnect && !openAIForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err) {
					h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", true)
				}
			} else {
				var failoverErr *service.UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					if c.Writer.Size() != writerSizeBeforeForward {
						h.handleAnthropicFailoverExhausted(c, failoverErr, true)
						return
					}
					// 池模式：同账号重试
					if failoverErr.RetryableOnSameAccount {
						retryDelay := sameAccountRetryDelayForAccount(account)
						if retryPlan, ok := planSameAccountRetry(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay); ok {
							reqLog.Warn("openai_messages.pool_mode_same_account_retry",
								zap.Int64("account_id", account.ID),
								zap.Int("upstream_status", failoverErr.StatusCode),
								zap.Int("retry_limit", retryPlan.RetryLimit),
								zap.Int("retry_count", retryPlan.RetryCount),
								zap.Duration("retry_delay", retryPlan.Delay),
								zap.Duration("retry_elapsed", retryPlan.Elapsed),
								zap.Duration("retry_max_elapsed", retryPlan.MaxElapsed),
							)
							select {
							case <-c.Request.Context().Done():
								return
							case <-time.After(retryPlan.Delay):
							}
							continue
						}
					}
					h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
					h.gatewayService.HandleOpenAIAccountFailoverSwitch(c.Request.Context(), apiKey.GroupID, sessionHash, account, failoverErr)
					h.gatewayService.RecordOpenAIAccountSwitch()
					modelRoutingLockedPriority = lockOpenAIModelRoutingFailoverPriority(
						modelRoutingLockedPriority,
						account,
						failoverErr,
						h.gatewayService == nil || h.gatewayService.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(c.Request.Context()),
					)
					failedAccountIDs[account.ID] = struct{}{}
					lastFailoverErr = failoverErr
					if switchCount >= maxAccountSwitches {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					switchCount++
					if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount) {
						h.handleAnthropicFailoverExhausted(c, failoverErr, streamStarted)
						return
					}
					reqLog.Warn("openai_messages.upstream_failover_switching",
						zap.Int64("account_id", account.ID),
						zap.Int("upstream_status", failoverErr.StatusCode),
						zap.Int("switch_count", switchCount),
						zap.Int("max_switches", maxAccountSwitches),
					)
					continue
				}
				if result != nil && result.ClientDisconnect {
					reqLog.Info("openai_messages.client_disconnected",
						zap.Int64("account_id", account.ID),
						zap.Error(err),
					)
					return
				}
				if service.GetOpsCyberPolicy(c) == nil {
					h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
				}
				wroteFallback := h.ensureAnthropicErrorResponse(c, streamStarted)
				h.recordCyberPolicyUsageIfMarked(
					c.Request.Context(),
					c,
					apiKey,
					account,
					subscription,
					reqModel,
					reqStream,
					GetInboundEndpoint(c),
					GetUpstreamEndpoint(c, account.Platform),
					service.HashUsageRequestPayload(body),
					channelMappingMsg.ToUsageFields(reqModel, ""),
				)
				reqLog.Warn("openai_messages.forward_failed",
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Error(err),
				)
				return
			}
		}
		successfulOutcome := true
		if result != nil {
			var neutralOutcome bool
			successfulOutcome, neutralOutcome = classifyOpenAIResponsesForwardResultWithError(result, err)
			if !successfulOutcome && service.GetOpsCyberPolicy(c) != nil {
				neutralOutcome = true
			}
			if !successfulOutcome {
				result.FirstTokenMs = nil
			}
			switch {
			case successfulOutcome:
				h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, true, result.FirstTokenMs)
			case neutralOutcome:
			default:
				h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
			}
			if !successfulOutcome && !openAIEdgeUsageIsBillable(result.Usage) && service.GetOpsCyberPolicy(c) == nil {
				return
			}
		} else {
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, true, nil)
			return
		}

		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		requestPayloadHash := service.HashUsageRequestPayload(body)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
		quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)

		cyberBlocked := result.CyberBlocked || service.GetOpsCyberPolicy(c) != nil
		h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
			if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
				Result:                  result,
				APIKey:                  apiKey,
				User:                    apiKey.User,
				Account:                 account,
				Subscription:            subscription,
				QuotaPlatform:           quotaPlatform,
				InboundEndpoint:         inboundEndpoint,
				UpstreamEndpoint:        upstreamEndpoint,
				UserAgent:               userAgent,
				IPAddress:               clientIP,
				RequestPayloadHash:      requestPayloadHash,
				PromptCacheAffinityHash: sessionHash,
				PromptCacheGroupID:      apiKey.GroupID,
				SkipSuccessSideEffects:  !successfulOutcome,
				APIKeyService:           h.apiKeyService,
				ChannelUsageFields:      channelMappingMsg.ToUsageFields(reqModel, result.UpstreamModel),
				CyberBlocked:            cyberBlocked,
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.messages"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai_messages.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai_messages.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

func resolveOpenAIMessagesMetadataSession(sessionHash, promptCacheKey, reqModel string, body []byte) (string, string) {
	// Anthropic metadata.user_id 只作为账号粘性信号。上游 GPT/Codex 缓存键
	// 交给 ForwardAsAnthropic 从 cache_control 或完整消息 digest 派生，避免
	// 固定 metadata key 压住后续 turn 的缓存滚动。
	if sessionHash != "" {
		return sessionHash, promptCacheKey
	}
	if userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String()); userID != "" {
		seed := reqModel + "-" + userID
		sessionHash = service.DeriveSessionHashFromSeed(seed)
	}
	return sessionHash, promptCacheKey
}

// anthropicErrorResponse writes an error in Anthropic Messages API format.
func (h *OpenAIGatewayHandler) anthropicErrorResponse(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// anthropicStreamingAwareError handles errors that may occur during streaming,
// using Anthropic SSE error format.
func (h *OpenAIGatewayHandler) anthropicStreamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	if streamStarted {
		flusher, ok := c.Writer.(http.Flusher)
		if ok {
			errPayload, _ := json.Marshal(gin.H{
				"type": "error",
				"error": gin.H{
					"type":    errType,
					"message": message,
				},
			})
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errPayload) //nolint:errcheck
			flusher.Flush()
		}
		return
	}
	h.anthropicErrorResponse(c, status, errType, message)
}

// handleAnthropicFailoverExhausted maps upstream failover errors to Anthropic format.
func (h *OpenAIGatewayHandler) handleAnthropicFailoverExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted bool) {
	status, errType, errMsg := h.mapUpstreamError(failoverErr.StatusCode)
	h.anthropicStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

// ensureAnthropicErrorResponse writes a fallback Anthropic error if no response was written.
func (h *OpenAIGatewayHandler) ensureAnthropicErrorResponse(c *gin.Context, streamStarted bool) bool {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
	return true
}

func (h *OpenAIGatewayHandler) validateFunctionCallOutputRequest(c *gin.Context, body []byte, reqLog *zap.Logger) bool {
	if !gjson.GetBytes(body, `input.#(type=="function_call_output")`).Exists() {
		return true
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		// 保持原有容错语义：解析失败时跳过预校验，沿用后续上游校验结果。
		return true
	}

	c.Set(service.OpenAIParsedRequestBodyKey, reqBody)
	validation := service.ValidateFunctionCallOutputContext(reqBody)
	if !validation.HasFunctionCallOutput {
		return true
	}

	previousResponseID, _ := reqBody["previous_response_id"].(string)
	if strings.TrimSpace(previousResponseID) != "" || validation.HasToolCallContext {
		return true
	}

	if validation.HasFunctionCallOutputMissingCallID {
		reqLog.Warn("openai.request_validation_failed",
			zap.String("reason", "function_call_output_missing_call_id"),
		)
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "function_call_output requires call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2")
		return false
	}
	if validation.HasItemReferenceForAllCallIDs {
		return true
	}

	reqLog.Warn("openai.request_validation_failed",
		zap.String("reason", "function_call_output_missing_item_reference"),
	)
	h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "function_call_output requires item_reference ids matching each call_id on HTTP requests; continuation via previous_response_id is only supported on Responses WebSocket v2")
	return false
}

func (h *OpenAIGatewayHandler) acquireResponsesUserSlot(
	c *gin.Context,
	userID int64,
	userConcurrency int,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
) (func(), bool) {
	ctx := c.Request.Context()
	apiKeyID := int64(0)
	if apiKey, ok := middleware2.GetAPIKeyFromContext(c); ok && apiKey != nil {
		apiKeyID = apiKey.ID
	}
	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, userID, userConcurrency, apiKeyID)
	if err != nil {
		reqLog.Warn("openai.user_slot_acquire_failed", zap.Error(err))
		h.handleConcurrencyError(c, err, "user", *streamStarted)
		return nil, false
	}
	if userAcquired {
		return wrapReleaseOnDone(ctx, userReleaseFunc), true
	}

	maxWait := service.CalculateMaxWait(userConcurrency)
	canWait, waitErr := h.concurrencyHelper.IncrementWaitCount(ctx, userID, maxWait)
	if waitErr != nil {
		reqLog.Warn("openai.user_wait_counter_increment_failed", zap.Error(waitErr))
		// 按现有降级语义：等待计数异常时放行后续抢槽流程
	} else if !canWait {
		reqLog.Info("openai.user_wait_queue_full", zap.Int("max_wait", maxWait))
		h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Too many pending requests, please retry later")
		return nil, false
	}

	waitCounted := waitErr == nil && canWait
	defer func() {
		if waitCounted {
			h.concurrencyHelper.DecrementWaitCount(ctx, userID)
		}
	}()

	slotWaitStart := time.Now()
	userReleaseFunc, err = h.concurrencyHelper.AcquireUserSlotWithWait(c, userID, userConcurrency, reqStream, streamStarted)
	service.SetOpsLatencyMsOnce(c, service.OpsSlotWaitMsKey, time.Since(slotWaitStart).Milliseconds())
	if err != nil {
		reqLog.Warn("openai.user_slot_acquire_failed_after_wait", zap.Error(err))
		h.handleConcurrencyError(c, err, "user", *streamStarted)
		return nil, false
	}

	// 槽位获取成功后，立刻退出等待计数。
	if waitCounted {
		h.concurrencyHelper.DecrementWaitCount(ctx, userID)
		waitCounted = false
	}
	return wrapReleaseOnDone(ctx, userReleaseFunc), true
}

func (h *OpenAIGatewayHandler) acquireResponsesAccountSlot(
	c *gin.Context,
	groupID *int64,
	sessionHash string,
	selection *service.AccountSelectionResult,
	reqStream bool,
	streamStarted *bool,
	reqLog *zap.Logger,
) openAIAccountSlotAcquireResult {
	if selection == nil || selection.Account == nil {
		markOpsRoutingCapacityLimited(c)
		h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", *streamStarted)
		return openAIAccountSlotAcquireResult{Err: errors.New("no available accounts")}
	}

	ctx := c.Request.Context()
	account := selection.Account
	if selection.Acquired {
		return openAIAccountSlotAcquireResult{
			ReleaseFunc: wrapReleaseOnDone(ctx, selection.ReleaseFunc),
			Acquired:    true,
		}
	}
	if selection.WaitPlan == nil {
		markOpsRoutingCapacityLimited(c)
		h.handleStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available accounts", *streamStarted)
		return openAIAccountSlotAcquireResult{Err: errors.New("no account wait plan")}
	}

	fastReleaseFunc, fastAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(
		ctx,
		account.ID,
		selection.WaitPlan.MaxConcurrency,
	)
	if err != nil {
		reqLog.Warn("openai.account_slot_quick_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		h.handleConcurrencyError(c, err, "account", *streamStarted)
		return openAIAccountSlotAcquireResult{Err: err}
	}
	if fastAcquired {
		if err := h.gatewayService.BindStickySession(ctx, groupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}
		return openAIAccountSlotAcquireResult{
			ReleaseFunc: wrapReleaseOnDone(ctx, fastReleaseFunc),
			Acquired:    true,
		}
	}

	canWait, waitErr := h.concurrencyHelper.IncrementAccountWaitCount(ctx, account.ID, selection.WaitPlan.MaxWaiting)
	if waitErr != nil {
		reqLog.Warn("openai.account_wait_counter_increment_failed", zap.Int64("account_id", account.ID), zap.Error(waitErr))
	} else if !canWait {
		reqLog.Info("openai.account_wait_queue_full",
			zap.Int64("account_id", account.ID),
			zap.Int("max_waiting", selection.WaitPlan.MaxWaiting),
		)
		if !*streamStarted {
			return openAIAccountSlotAcquireResult{
				CapacityMiss: true,
				Reason:       "account_wait_queue_full",
			}
		}
		h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Too many pending requests, please retry later", *streamStarted)
		return openAIAccountSlotAcquireResult{Err: errors.New("account wait queue full")}
	}

	accountWaitCounted := waitErr == nil && canWait
	releaseWait := func() {
		if accountWaitCounted {
			h.concurrencyHelper.DecrementAccountWaitCount(ctx, account.ID)
			accountWaitCounted = false
		}
	}
	defer releaseWait()

	slotWaitStart := time.Now()
	accountReleaseFunc, err := h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(
		c,
		account.ID,
		selection.WaitPlan.MaxConcurrency,
		selection.WaitPlan.Timeout,
		reqStream,
		streamStarted,
	)
	service.SetOpsLatencyMsOnce(c, service.OpsSlotWaitMsKey, time.Since(slotWaitStart).Milliseconds())
	if err != nil {
		reqLog.Warn("openai.account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		var concurrencyErr *ConcurrencyError
		if !*streamStarted && errors.As(err, &concurrencyErr) && concurrencyErr.SlotType == "account" {
			return openAIAccountSlotAcquireResult{
				CapacityMiss: true,
				Reason:       "account_slot_wait_timeout",
				Err:          err,
			}
		}
		h.handleConcurrencyError(c, err, "account", *streamStarted)
		return openAIAccountSlotAcquireResult{Err: err}
	}

	// Slot acquired: no longer waiting in queue.
	releaseWait()
	if err := h.gatewayService.BindStickySession(ctx, groupID, sessionHash, account.ID); err != nil {
		reqLog.Warn("openai.bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
	}
	return openAIAccountSlotAcquireResult{
		ReleaseFunc: wrapReleaseOnDone(ctx, accountReleaseFunc),
		Acquired:    true,
	}
}

// ResponsesWebSocket handles OpenAI Responses API WebSocket ingress endpoint
// GET /openai/v1/responses (Upgrade: websocket)
func (h *OpenAIGatewayHandler) ResponsesWebSocket(c *gin.Context) {
	if !isOpenAIWSUpgradeRequest(c.Request) {
		h.errorResponse(c, http.StatusUpgradeRequired, "invalid_request_error", "WebSocket upgrade required (Upgrade: websocket)")
		return
	}
	setOpenAIClientTransportWS(c)

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}

	reqLog := requestLogger(
		c,
		"handler.openai_gateway.responses_ws",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.Bool("openai_ws_mode", true),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}
	reqLog.Info("openai.websocket_ingress_started")
	clientIP := ip.GetClientIP(c)
	userAgent := strings.TrimSpace(c.GetHeader("User-Agent"))

	wsConn, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{
		CompressionMode: coderws.CompressionContextTakeover,
	})
	if err != nil {
		reqLog.Warn("openai.websocket_accept_failed",
			zap.Error(err),
			zap.String("client_ip", clientIP),
			zap.String("request_user_agent", userAgent),
			zap.String("upgrade_header", strings.TrimSpace(c.GetHeader("Upgrade"))),
			zap.String("connection_header", strings.TrimSpace(c.GetHeader("Connection"))),
			zap.String("sec_websocket_version", strings.TrimSpace(c.GetHeader("Sec-WebSocket-Version"))),
			zap.Bool("has_sec_websocket_key", strings.TrimSpace(c.GetHeader("Sec-WebSocket-Key")) != ""),
		)
		return
	}
	defer func() {
		_ = wsConn.CloseNow()
	}()
	wsConn.SetReadLimit(service.ResolveOpenAIWSClientReadLimitBytes(h.cfg))

	ctx := c.Request.Context()
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	msgType, firstMessage, err := wsConn.Read(readCtx)
	cancel()
	if err != nil {
		closeStatus, closeReason := summarizeWSCloseErrorForLog(err)
		reqLog.Warn("openai.websocket_read_first_message_failed",
			zap.Error(err),
			zap.String("client_ip", clientIP),
			zap.String("close_status", closeStatus),
			zap.String("close_reason", closeReason),
			zap.Duration("read_timeout", 30*time.Second),
		)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "missing first response.create message")
		return
	}
	if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "unsupported websocket message type")
		return
	}
	if !gjson.ValidBytes(firstMessage) {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "invalid JSON payload")
		return
	}

	reqModel := strings.TrimSpace(gjson.GetBytes(firstMessage, "model").String())
	if reqModel == "" {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "model is required in first response.create payload")
		return
	}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(firstMessage, "previous_response_id").String())
	previousResponseIDKind := service.ClassifyOpenAIPreviousResponseIDKind(previousResponseID)
	if previousResponseID != "" && previousResponseIDKind == service.OpenAIPreviousResponseIDKindMessageID {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "previous_response_id must be a response.id (resp_*), not a message id")
		return
	}
	reqLog = reqLog.With(
		zap.Bool("ws_ingress", true),
		zap.String("model", reqModel),
		zap.Bool("has_previous_response_id", previousResponseID != ""),
		zap.String("previous_response_id_kind", previousResponseIDKind),
	)
	setOpsRequestContext(c, reqModel, true)
	setOpsEndpointContext(c, "", int16(service.RequestTypeWSV2))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, reqModel, firstMessage); decision != nil && decision.Blocked {
		writeContentModerationWSError(ctx, wsConn, decision)
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, decision.Message)
		return
	}

	imageIntent := service.IsImageGenerationIntent("/v1/responses", reqModel, firstMessage)
	if imageIntent && !service.GroupAllowsImageGeneration(apiKey.Group) {
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, service.ImageGenerationPermissionMessage())
		return
	}
	requestCtx := ctx
	if imageIntent {
		requestCtx = service.WithOpenAIImageGenerationIntent(requestCtx)
	}

	// 解析渠道级模型映射
	channelMappingWS, _ := h.gatewayService.ResolveChannelMappingAndRestrict(requestCtx, apiKey.GroupID, reqModel)

	var currentUserRelease func()
	var currentAccountRelease func()
	releaseAccountSlot := func() {
		if currentAccountRelease != nil {
			currentAccountRelease()
			currentAccountRelease = nil
		}
	}
	releaseTurnSlots := func() {
		releaseAccountSlot()
		if currentUserRelease != nil {
			currentUserRelease()
			currentUserRelease = nil
		}
	}
	// 必须尽早注册，确保任何 early return 都能释放已获取的并发槽位。
	defer releaseTurnSlots()

	userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, subject.UserID, subject.Concurrency, apiKey.ID)
	if err != nil {
		reqLog.Warn("openai.websocket_user_slot_acquire_failed", zap.Error(err))
		closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire user concurrency slot")
		return
	}
	if !userAcquired {
		closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "too many concurrent requests, please retry later")
		return
	}
	currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
	ensureUserSlotHeld := func() bool {
		if currentUserRelease != nil {
			return true
		}
		userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, subject.UserID, subject.Concurrency, apiKey.ID)
		if err != nil {
			reqLog.Warn("openai.websocket_user_slot_reacquire_failed", zap.Error(err))
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire user concurrency slot")
			return false
		}
		if !userAcquired {
			closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "too many concurrent requests, please retry later")
			return false
		}
		currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
		return true
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	if err := h.billingCacheService.CheckBillingEligibility(requestCtx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(requestCtx, apiKey)); err != nil {
		reqLog.Info("openai.websocket_billing_eligibility_check_failed", zap.Error(err))
		closeOpenAIClientWS(wsConn, coderws.StatusPolicyViolation, "billing check failed")
		return
	}

	sessionHash := h.gatewayService.GenerateSessionHashWithFallback(
		c,
		firstMessage,
		openAIWSIngressFallbackSessionSeed(subject.UserID, apiKey.ID, apiKey.GroupID),
	)
	maxAccountSwitches := h.maxAccountSwitches
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	capacitySkippedIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	sameAccountRetryStartedAt := make(map[int64]time.Time)
	var lastFailoverErr *service.UpstreamFailoverError
	modelRoutingLockedPriority := -1

	for {
		excludedAccountIDs := mergeOpenAIAccountExclusions(failedAccountIDs, capacitySkippedIDs)
		reqLog.Debug("openai.websocket_account_selecting", zap.Int("excluded_account_count", len(excludedAccountIDs)))
		selection, scheduleDecision, err := h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(
			requestCtx,
			apiKey.GroupID,
			previousResponseID,
			sessionHash,
			reqModel,
			excludedAccountIDs,
			service.OpenAIUpstreamTransportResponsesWebsocketV2Ingress,
			service.OpenAIEndpointCapabilityChatCompletions,
			false,
			service.PlatformOpenAI,
			modelRoutingLockedPriority,
		)
		if err != nil {
			reqLog.Warn("openai.websocket_account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if lastFailoverErr != nil {
				closeOpenAIWSFailoverExhausted(wsConn, lastFailoverErr)
			} else {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "no available account")
			}
			return
		}
		if selection == nil || selection.Account == nil {
			if lastFailoverErr != nil {
				closeOpenAIWSFailoverExhausted(wsConn, lastFailoverErr)
			} else {
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "no available account")
			}
			return
		}

		account := selection.Account
		markSameAccountAttemptStart(sameAccountRetryStartedAt, account, time.Now())
		accountMaxConcurrency := account.Concurrency
		if selection.WaitPlan != nil && selection.WaitPlan.MaxConcurrency > 0 {
			accountMaxConcurrency = selection.WaitPlan.MaxConcurrency
		}
		accountReleaseFunc := selection.ReleaseFunc
		if !selection.Acquired {
			if selection.WaitPlan == nil {
				if previousResponseID == "" {
					capacitySkippedIDs[account.ID] = struct{}{}
					reqLog.Info("openai.websocket_account_capacity_skip",
						zap.Int64("account_id", account.ID),
						zap.String("reason", "account_slot_not_available"),
						zap.Int("capacity_skipped_count", len(capacitySkippedIDs)),
					)
					continue
				}
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
				return
			}
			fastReleaseFunc, fastAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(
				ctx,
				account.ID,
				selection.WaitPlan.MaxConcurrency,
			)
			if err != nil {
				reqLog.Warn("openai.websocket_account_slot_acquire_failed", zap.Int64("account_id", account.ID), zap.Error(err))
				closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to acquire account concurrency slot")
				return
			}
			if !fastAcquired {
				if previousResponseID == "" {
					capacitySkippedIDs[account.ID] = struct{}{}
					reqLog.Info("openai.websocket_account_capacity_skip",
						zap.Int64("account_id", account.ID),
						zap.String("reason", "account_slot_busy"),
						zap.Int("capacity_skipped_count", len(capacitySkippedIDs)),
					)
					continue
				}
				closeOpenAIClientWS(wsConn, coderws.StatusTryAgainLater, "account is busy, please retry later")
				return
			}
			accountReleaseFunc = fastReleaseFunc
		}
		currentAccountRelease = wrapReleaseOnDone(ctx, accountReleaseFunc)
		if err := h.gatewayService.BindStickySession(ctx, apiKey.GroupID, sessionHash, account.ID); err != nil {
			reqLog.Warn("openai.websocket_bind_sticky_session_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		}

		token, _, err := h.gatewayService.GetAccessToken(ctx, account)
		if err != nil {
			reqLog.Warn("openai.websocket_get_access_token_failed", zap.Int64("account_id", account.ID), zap.Error(err))
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "failed to get access token")
			return
		}

		reqLog.Debug("openai.websocket_account_selected",
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("schedule_layer", scheduleDecision.Layer),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
		)

		hooks := &service.OpenAIWSIngressHooks{
			InitialRequestModel: reqModel,
			BeforeRequest: func(turn int, payload []byte, originalModel string) error {
				if turn == 1 {
					return nil
				}
				if !gjson.ValidBytes(payload) {
					return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, "invalid websocket request payload", errors.New("invalid json"))
				}
				model := strings.TrimSpace(originalModel)
				if model == "" {
					model = strings.TrimSpace(gjson.GetBytes(payload, "model").String())
				}
				if model == "" {
					model = reqModel
				}
				if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIResponses, model, payload); decision != nil && decision.Blocked {
					writeContentModerationWSError(ctx, wsConn, decision)
					return service.NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, decision.Message, nil)
				}
				return nil
			},
			BeforeTurn: func(turn int) error {
				if turn == 1 {
					return nil
				}
				// 防御式清理：避免异常路径下旧槽位覆盖导致泄漏。
				releaseTurnSlots()
				// 非首轮 turn 需要重新抢占并发槽位，避免长连接空闲占槽。
				userReleaseFunc, userAcquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(ctx, subject.UserID, subject.Concurrency, apiKey.ID)
				if err != nil {
					return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "failed to acquire user concurrency slot", err)
				}
				if !userAcquired {
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "too many concurrent requests, please retry later", nil)
				}
				accountReleaseFunc, accountAcquired, err := h.concurrencyHelper.TryAcquireAccountSlot(ctx, account.ID, accountMaxConcurrency)
				if err != nil {
					if userReleaseFunc != nil {
						userReleaseFunc()
					}
					return service.NewOpenAIWSClientCloseError(coderws.StatusInternalError, "failed to acquire account concurrency slot", err)
				}
				if !accountAcquired {
					if userReleaseFunc != nil {
						userReleaseFunc()
					}
					return service.NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "account is busy, please retry later", nil)
				}
				currentUserRelease = wrapReleaseOnDone(ctx, userReleaseFunc)
				currentAccountRelease = wrapReleaseOnDone(ctx, accountReleaseFunc)
				return nil
			},
			AfterTurn: func(turn int, result *service.OpenAIForwardResult, turnErr error) {
				releaseTurnSlots()
				if turnErr != nil && result != nil && result.ImageCount > 0 {
					reqLog.Warn("openai.websocket_partial_error_with_image_result",
						zap.Int64("account_id", account.ID),
						zap.Int("image_count", result.ImageCount),
						zap.Error(turnErr),
					)
				}
				if result == nil {
					return
				}
				if account.Type == service.AccountTypeOAuth {
					h.gatewayService.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, result.ResponseHeaders)
				}
				routingModel := strings.TrimSpace(result.Model)
				if routingModel == "" {
					routingModel = reqModel
				}
				terminalType := strings.ToLower(strings.TrimSpace(result.TerminalEventType))
				successfulTerminal, neutralOutcome := classifyOpenAIResponsesForwardResultWithError(result, turnErr)
				cyberBlocked := result.CyberBlocked
				switch {
				case successfulTerminal:
					h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, routingModel, true, result.FirstTokenMs)
				case neutralOutcome:
					// Client disconnect, cyber policy, and incomplete/cancelled turns are
					// billable when usage exists but are not account-health samples.
				case terminalType == "response.failed":
					// HTTP bridge returns result+error after forwarding the safe failure
					// frame; its outer error path records the failure exactly once.
					if turnErr == nil && !cyberBlocked {
						h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, routingModel, false, nil)
					}
				default:
					if turnErr == nil {
						h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, routingModel, false, nil)
					}
				}
				if !successfulTerminal {
					// Failed, incomplete and protocol-truncated turns must not enter
					// successful TTFT statistics. Usage remains billable when present.
					result.FirstTokenMs = nil
				}
				if !successfulTerminal && !openAIEdgeUsageIsBillable(result.Usage) {
					return
				}
				inboundEndpoint := GetInboundEndpoint(c)
				upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
				quotaPlatform := service.QuotaPlatform(ctx, apiKey)
				h.submitOpenAIUsageRecordTask(ctx, result, func(taskCtx context.Context) {
					if err := h.gatewayService.RecordUsage(taskCtx, &service.OpenAIRecordUsageInput{
						Result:                  result,
						APIKey:                  apiKey,
						User:                    apiKey.User,
						Account:                 account,
						Subscription:            subscription,
						QuotaPlatform:           quotaPlatform,
						InboundEndpoint:         inboundEndpoint,
						UpstreamEndpoint:        upstreamEndpoint,
						UserAgent:               userAgent,
						IPAddress:               clientIP,
						RequestPayloadHash:      service.HashUsageRequestPayload(firstMessage),
						PromptCacheAffinityHash: sessionHash,
						PromptCacheGroupID:      apiKey.GroupID,
						SkipSuccessSideEffects:  !successfulTerminal,
						APIKeyService:           h.apiKeyService,
						ChannelUsageFields:      channelMappingWS.ToUsageFields(reqModel, result.UpstreamModel),
						CyberBlocked:            cyberBlocked,
					}); err != nil {
						reqLog.Error("openai.websocket_record_usage_failed",
							zap.Int64("account_id", account.ID),
							zap.String("request_id", result.RequestID),
							zap.Error(err),
						)
					}
				})
			},
		}

		// 应用渠道模型映射到 WebSocket 首条消息
		wsFirstMessage := firstMessage
		if channelMappingWS.Mapped {
			wsFirstMessage = h.gatewayService.ReplaceModelInBody(firstMessage, channelMappingWS.MappedModel)
		}

		if err := h.gatewayService.ProxyResponsesWebSocketFromClient(ctx, c, wsConn, account, token, wsFirstMessage, hooks); err != nil {
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				releaseAccountSlot()
				if failoverErr.RetryableOnSameAccount {
					retryDelay := sameAccountRetryDelayForAccount(account)
					if retryPlan, ok := planSameAccountRetry(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay); ok {
						reqLog.Warn("openai.websocket_pool_mode_same_account_retry",
							zap.Int64("account_id", account.ID),
							zap.Int("upstream_status", failoverErr.StatusCode),
							zap.Int("retry_limit", retryPlan.RetryLimit),
							zap.Int("retry_count", retryPlan.RetryCount),
							zap.Duration("retry_delay", retryPlan.Delay),
							zap.Duration("retry_elapsed", retryPlan.Elapsed),
							zap.Duration("retry_max_elapsed", retryPlan.MaxElapsed),
						)
						if !ensureUserSlotHeld() {
							return
						}
						select {
						case <-requestCtx.Done():
							return
						case <-time.After(retryPlan.Delay):
						}
						continue
					}
				}
				h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
				modelRoutingLockedPriority = lockOpenAIModelRoutingFailoverPriority(
					modelRoutingLockedPriority,
					account,
					failoverErr,
					h.gatewayService == nil || h.gatewayService.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(c.Request.Context()),
				)
				failedAccountIDs[account.ID] = struct{}{}
				lastFailoverErr = failoverErr
				if switchCount >= maxAccountSwitches {
					closeOpenAIWSFailoverExhausted(wsConn, failoverErr)
					return
				}
				switchCount++
				if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount) {
					closeOpenAIWSFailoverExhausted(wsConn, failoverErr)
					return
				}
				h.gatewayService.HandleOpenAIAccountFailoverSwitch(c.Request.Context(), apiKey.GroupID, sessionHash, account, failoverErr)
				h.gatewayService.RecordOpenAIAccountSwitch()
				reqLog.Warn("openai.websocket_upstream_failover_switching",
					zap.Int64("account_id", account.ID),
					zap.Int("upstream_status", failoverErr.StatusCode),
					zap.Int("switch_count", switchCount),
					zap.Int("max_switches", maxAccountSwitches),
				)
				if !ensureUserSlotHeld() {
					return
				}
				continue
			}

			closeStatus, closeReason := summarizeWSCloseErrorForLog(err)
			reqLog.Warn("openai.websocket_proxy_failed",
				zap.Int64("account_id", account.ID),
				zap.Error(err),
				zap.String("close_status", closeStatus),
				zap.String("close_reason", closeReason),
			)
			var closeErr *service.OpenAIWSClientCloseError
			if errors.As(err, &closeErr) {
				closeOpenAIClientWS(wsConn, closeErr.StatusCode(), closeErr.Reason())
				return
			}
			if service.IsOpenAIWSClientDisconnectError(err) {
				return
			}
			if shouldReportOpenAIWSProxyFailure(c, err) {
				h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
			}
			closeOpenAIClientWS(wsConn, coderws.StatusInternalError, "upstream websocket proxy failed")
			return
		}
		reqLog.Info("openai.websocket_ingress_closed", zap.Int64("account_id", account.ID))
		return
	}

}

func shouldReportOpenAIWSProxyFailure(c *gin.Context, err error) bool {
	return err != nil && !service.IsOpenAIWSClientDisconnectError(err) && service.GetOpsCyberPolicy(c) == nil
}

func (h *OpenAIGatewayHandler) recoverResponsesPanic(c *gin.Context, streamStarted *bool) {
	recovered := recover()
	if recovered == nil {
		return
	}

	started := false
	if streamStarted != nil {
		started = *streamStarted
	}
	wroteFallback := h.ensureForwardErrorResponse(c, started)
	requestLogger(c, "handler.openai_gateway.responses").Error(
		"openai.responses_panic_recovered",
		zap.Bool("fallback_error_response_written", wroteFallback),
		zap.Any("panic", recovered),
		zap.ByteString("stack", debug.Stack()),
	)
}

// recoverAnthropicMessagesPanic recovers from panics in the Anthropic Messages
// handler and returns an Anthropic-formatted error response.
func (h *OpenAIGatewayHandler) recoverAnthropicMessagesPanic(c *gin.Context, streamStarted *bool) {
	recovered := recover()
	if recovered == nil {
		return
	}

	started := streamStarted != nil && *streamStarted
	requestLogger(c, "handler.openai_gateway.messages").Error(
		"openai.messages_panic_recovered",
		zap.Bool("stream_started", started),
		zap.Any("panic", recovered),
		zap.ByteString("stack", debug.Stack()),
	)
	if !started {
		h.anthropicErrorResponse(c, http.StatusInternalServerError, "api_error", "Internal server error")
	}
}

func (h *OpenAIGatewayHandler) ensureResponsesDependencies(c *gin.Context, reqLog *zap.Logger) bool {
	missing := h.missingResponsesDependencies()
	if len(missing) == 0 {
		return true
	}

	if reqLog == nil {
		reqLog = requestLogger(c, "handler.openai_gateway.responses")
	}
	reqLog.Error("openai.handler_dependencies_missing", zap.Strings("missing_dependencies", missing))

	if c != nil && c.Writer != nil && !c.Writer.Written() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{
				"type":    "api_error",
				"message": "Service temporarily unavailable",
			},
		})
	}
	return false
}

func (h *OpenAIGatewayHandler) missingResponsesDependencies() []string {
	missing := make([]string, 0, 5)
	if h == nil {
		return append(missing, "handler")
	}
	if h.gatewayService == nil {
		missing = append(missing, "gatewayService")
	}
	if h.billingCacheService == nil {
		missing = append(missing, "billingCacheService")
	}
	if h.apiKeyService == nil {
		missing = append(missing, "apiKeyService")
	}
	if h.concurrencyHelper == nil || h.concurrencyHelper.concurrencyService == nil {
		missing = append(missing, "concurrencyHelper")
	}
	return missing
}

func getContextInt64(c *gin.Context, key string) (int64, bool) {
	if c == nil || key == "" {
		return 0, false
	}
	v, ok := c.Get(key)
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case int32:
		return int64(t), true
	case float64:
		return int64(t), true
	default:
		return 0, false
	}
}

func (h *OpenAIGatewayHandler) submitUsageRecordTask(parent context.Context, task service.UsageRecordTask) {
	if task == nil {
		return
	}
	task = wrapUsageRecordTaskContext(parent, task)
	if h.usageRecordWorkerPool != nil {
		h.usageRecordWorkerPool.Submit(task)
		return
	}
	// 回退路径：worker 池未注入时同步执行，避免退回到无界 goroutine 模式。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.responses"),
				zap.Any("panic", recovered),
			).Error("openai.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

func (h *OpenAIGatewayHandler) submitOpenAIUsageRecordTask(parent context.Context, result *service.OpenAIForwardResult, task service.UsageRecordTask) {
	if result != nil && result.ImageCount > 0 {
		h.submitMandatoryUsageRecordTask(parent, task)
		return
	}
	h.submitUsageRecordTask(parent, task)
}

func (h *OpenAIGatewayHandler) recordCyberPolicyUsageIfMarked(
	parent context.Context,
	c *gin.Context,
	apiKey *service.APIKey,
	account *service.Account,
	subscription *service.UserSubscription,
	model string,
	stream bool,
	inboundEndpoint string,
	upstreamEndpoint string,
	requestPayloadHash string,
	channelFields service.ChannelUsageFields,
) {
	if h == nil || h.gatewayService == nil {
		return
	}
	mark := service.GetOpsCyberPolicy(c)
	if mark == nil {
		return
	}
	requestID := ""
	if c != nil && c.Writer != nil {
		requestID = c.Writer.Header().Get("X-Request-Id")
	}
	userAgent := ""
	clientIP := ""
	if c != nil {
		userAgent = c.GetHeader("User-Agent")
		clientIP = ip.GetClientIP(c)
	}
	h.submitOpenAIUsageRecordTask(parent, nil, func(ctx context.Context) {
		h.gatewayService.RecordCyberPolicyUsageLog(ctx, service.CyberPolicyUsageInput{
			APIKey:             apiKey,
			Account:            account,
			Subscription:       subscription,
			RequestID:          requestID,
			Model:              model,
			Stream:             stream,
			InputTokens:        mark.UpstreamInTok,
			OutputTokens:       mark.UpstreamOutTok,
			InboundEndpoint:    inboundEndpoint,
			UpstreamEndpoint:   upstreamEndpoint,
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			RequestPayloadHash: requestPayloadHash,
			APIKeyService:      h.apiKeyService,
			ChannelUsageFields: channelFields,
		})
	})
}

func (h *OpenAIGatewayHandler) submitMandatoryUsageRecordTask(parent context.Context, task service.UsageRecordTask) {
	if task == nil {
		return
	}
	task = wrapUsageRecordTaskContext(parent, task)
	if h.usageRecordWorkerPool != nil {
		if mode := h.usageRecordWorkerPool.Submit(task); mode != service.UsageRecordSubmitModeDropped {
			return
		}
		logger.L().With(
			zap.String("component", "handler.openai_gateway.usage"),
		).Warn("openai.usage_record_task_mandatory_sync_fallback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.usage"),
				zap.Any("panic", recovered),
			).Error("openai.usage_record_task_panic_recovered")
		}
	}()
	task(ctx)
}

func (h *OpenAIGatewayHandler) acquireImageGenerationSlot(c *gin.Context, streamStarted bool) (func(), bool) {
	if h == nil || h.cfg == nil || h.imageLimiter == nil {
		return nil, true
	}
	imageConcurrency := h.cfg.Gateway.ImageConcurrency
	wait := strings.TrimSpace(imageConcurrency.OverflowMode) == config.ImageConcurrencyOverflowModeWait
	release, acquired := h.imageLimiter.Acquire(
		c.Request.Context(),
		imageConcurrency.Enabled,
		imageConcurrency.MaxConcurrentRequests,
		wait,
		time.Duration(imageConcurrency.WaitTimeoutSeconds)*time.Second,
		imageConcurrency.MaxWaitingRequests,
	)
	if acquired {
		return release, true
	}
	h.handleStreamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "Image generation concurrency limit exceeded, please retry later", streamStarted)
	return nil, false
}

// handleConcurrencyError handles concurrency-related acquire errors.
func (h *OpenAIGatewayHandler) handleConcurrencyError(c *gin.Context, err error, slotType string, streamStarted bool) {
	status, errType, message := concurrencyErrorResponse(err, slotType)
	h.handleStreamingAwareError(c, status, errType, message, streamStarted)
}

func (h *OpenAIGatewayHandler) handleFailoverExhausted(c *gin.Context, failoverErr *service.UpstreamFailoverError, streamStarted bool) {
	statusCode := failoverErr.StatusCode
	responseBody := failoverErr.ResponseBody
	if service.IsOpenAIResponsesHealthProbe(c) && service.IsOpenAIHealthProbeEmptyErrorBody(responseBody) {
		message := service.OpenAIHealthProbeClientMessage()
		service.SetOpsUpstreamError(c, statusCode, message, "")
		h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", message, streamStarted)
		return
	}
	if service.IsOpenAISilentRefusalErrorBody(responseBody) {
		service.SetOpsUpstreamError(c, statusCode, service.OpenAISilentRefusalClientMessage(), "")
		h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", service.OpenAISilentRefusalClientMessage(), streamStarted)
		return
	}
	upstreamMsg := service.ExtractUpstreamErrorMessage(responseBody)
	if failoverErr.SkipPoolSoftCooldown &&
		(h.gatewayService == nil || h.gatewayService.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(c.Request.Context())) &&
		service.IsOpenAIPoolModelRoutingError(statusCode, upstreamMsg, responseBody) {
		service.SetOpsUpstreamError(c, statusCode, upstreamMsg, "")
		h.handleStreamingAwareError(c, http.StatusBadRequest, "invalid_request_error", service.OpenAIPoolModelRoutingClientMessage(), streamStarted)
		return
	}

	// 先检查透传规则
	if h.errorPassthroughService != nil && len(responseBody) > 0 {
		if rule := h.errorPassthroughService.MatchRule("openai", statusCode, responseBody); rule != nil {
			// 确定响应状态码
			respCode := statusCode
			if !rule.PassthroughCode && rule.ResponseCode != nil {
				respCode = *rule.ResponseCode
			}

			// 确定响应消息
			msg := "Upstream request failed"
			if !rule.PassthroughBody && rule.CustomMessage != nil {
				msg = *rule.CustomMessage
			}

			if rule.SkipMonitoring {
				c.Set(service.OpsSkipPassthroughKey, true)
			}

			h.handleStreamingAwareError(c, respCode, "upstream_error", msg, streamStarted)
			return
		}
	}

	// 记录原始上游状态码，以便 ops 错误日志捕获真实的上游错误
	service.SetOpsUpstreamError(c, statusCode, upstreamMsg, "")

	// 使用默认的错误映射
	status, errType, errMsg := h.mapUpstreamError(statusCode)
	h.handleStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

// handleFailoverExhaustedSimple 简化版本，用于没有响应体的情况
func (h *OpenAIGatewayHandler) handleFailoverExhaustedSimple(c *gin.Context, statusCode int, streamStarted bool) {
	status, errType, errMsg := h.mapUpstreamError(statusCode)
	service.SetOpsUpstreamError(c, statusCode, errMsg, "")
	h.handleStreamingAwareError(c, status, errType, errMsg, streamStarted)
}

func (h *OpenAIGatewayHandler) mapUpstreamError(statusCode int) (int, string, string) {
	switch statusCode {
	case 401:
		return http.StatusBadGateway, "upstream_error", "Upstream authentication failed, please contact administrator"
	case 403:
		return http.StatusBadGateway, "upstream_error", "Upstream access forbidden, please contact administrator"
	case 429:
		return http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded, please retry later"
	case 529:
		return http.StatusServiceUnavailable, "upstream_error", "Upstream service overloaded, please retry later"
	case 500, 502, 503, 504:
		return http.StatusBadGateway, "upstream_error", "Upstream service temporarily unavailable"
	default:
		return http.StatusBadGateway, "upstream_error", "Upstream request failed"
	}
}

// handleStreamingAwareError handles errors that may occur after streaming has started
func (h *OpenAIGatewayHandler) handleStreamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		streamStarted = true
	}
	if streamStarted {
		service.MarkOpsStreamError(c, status, errType, message)
		// /v1/responses 的严格 SDK（Codex CLI）要求终止事件必须属于
		// response.completed/failed/incomplete/cancelled 集合。
		// 通用 `event: error` 帧不被识别为终止事件，会导致
		// "stream closed before response.completed"。
		if inboundIsResponses(c) {
			if writeResponsesFailedSSE(c, errType, message) {
				return
			}
		}
		// Stream already started, send error as SSE event then close
		flusher, ok := c.Writer.(http.Flusher)
		if ok {
			// SSE 错误事件固定 schema，使用 Quote 直拼可避免额外 Marshal 分配。
			errorEvent := "event: error\ndata: " + `{"error":{"type":` + strconv.Quote(errType) + `,"message":` + strconv.Quote(message) + `}}` + "\n\n"
			if _, err := fmt.Fprint(c.Writer, errorEvent); err != nil {
				_ = c.Error(err)
			}
			flusher.Flush()
		}
		return
	}

	// Normal case: return JSON response with proper status code
	h.errorResponse(c, status, errType, message)
}

// ensureForwardErrorResponse 在 Forward 返回错误但尚未写响应时补写统一错误响应。
func (h *OpenAIGatewayHandler) ensureForwardErrorResponse(c *gin.Context, streamStarted bool) bool {
	if c == nil || c.Writer == nil {
		return false
	}
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		streamStarted = true
	}
	if service.IsResponseCommitted(c) {
		return false
	}
	// 旧实现在 Writer.Written 时直接 return false，导致 ping 已 flush 之后的
	// 上游错误（http2 timeout、连接中断等）完全无法把错误传给客户端——
	// HTTP 200 已锁死，TCP 直接 EOF，Codex CLI 报 "stream closed before response.completed"。
	// 这里改成：Writer 已写过时强制走 streamStarted 分支，让
	// handleStreamingAwareError 通过 SSE 发协议合规的 response.failed。
	if c.Writer.Written() {
		streamStarted = true
	}
	h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed", streamStarted)
	return true
}

func shouldLogOpenAIForwardFailureAsWarn(c *gin.Context, wroteFallback bool) bool {
	if wroteFallback {
		return false
	}
	if c == nil || c.Writer == nil {
		return false
	}
	return c.Writer.Written()
}

func openAIForwardErrorAlreadyCommunicated(c *gin.Context, writerSizeBeforeForward int, err error) bool {
	if c == nil || c.Writer == nil || err == nil {
		return false
	}
	if service.IsResponseCommitted(c) {
		return true
	}
	if service.OpenAICompactKeepaliveAdjustedWrittenSize(c) == writerSizeBeforeForward {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(c.Writer.Header().Get("Content-Type")))
	if contentType != "" && !strings.Contains(contentType, "text/event-stream") {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "upstream response failed") || strings.Contains(message, "response.failed")
}

// errorResponse returns OpenAI API format error response
func (h *OpenAIGatewayHandler) errorResponse(c *gin.Context, status int, errType, message string) {
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		service.MarkOpsStreamError(c, status, errType, message)
		if writeResponsesFailedSSE(c, errType, message) {
			return
		}
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

func (h *OpenAIGatewayHandler) openAICompactKeepaliveInterval() time.Duration {
	if h.cfg == nil || h.cfg.Gateway.StreamKeepaliveInterval <= 0 {
		return 0
	}
	return time.Duration(h.cfg.Gateway.StreamKeepaliveInterval) * time.Second
}

func setOpenAIClientTransportHTTP(c *gin.Context) {
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportHTTP)
}

func setOpenAIClientTransportWS(c *gin.Context) {
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
}

func ensureOpenAIPoolModeSessionHash(sessionHash string, account *service.Account) string {
	if sessionHash != "" || account == nil || !account.IsPoolMode() {
		return sessionHash
	}
	// 为当前请求生成一次性粘性会话键，确保同账号重试不会重新负载均衡到其他账号。
	return "openai-pool-retry-" + uuid.NewString()
}

func openAIWSIngressFallbackSessionSeed(userID, apiKeyID int64, groupID *int64) string {
	gid := int64(0)
	if groupID != nil {
		gid = *groupID
	}
	return fmt.Sprintf("openai_ws_ingress:%d:%d:%d", gid, userID, apiKeyID)
}

func isOpenAIWSUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(r.Header.Get("Connection"))), "upgrade")
}

func closeOpenAIClientWS(conn *coderws.Conn, status coderws.StatusCode, reason string) {
	if conn == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = conn.Close(status, reason)
	_ = conn.CloseNow()
}

func openAICyberPolicyErrorText(decision service.CyberPolicyDecision) (errType string, message string) {
	errType = strings.TrimSpace(decision.ErrorType)
	if errType == "" {
		errType = "invalid_request_error"
	}
	message = strings.TrimSpace(decision.Message)
	if message == "" {
		message = "upstream cyber_policy blocked this session"
	}
	return errType, message
}

func closeOpenAIWSFailoverExhausted(conn *coderws.Conn, failoverErr *service.UpstreamFailoverError) {
	if failoverErr == nil {
		closeOpenAIClientWS(conn, coderws.StatusInternalError, "upstream websocket proxy failed")
		return
	}
	switch failoverErr.StatusCode {
	case http.StatusTooManyRequests:
		closeOpenAIClientWS(conn, coderws.StatusTryAgainLater, "upstream rate limit exceeded, please retry later")
	case 529, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		closeOpenAIClientWS(conn, coderws.StatusTryAgainLater, "upstream service temporarily unavailable")
	case http.StatusUnauthorized, http.StatusForbidden:
		closeOpenAIClientWS(conn, coderws.StatusPolicyViolation, "upstream websocket authentication failed")
	default:
		closeOpenAIClientWS(conn, coderws.StatusInternalError, "upstream websocket proxy failed")
	}
}

func writeContentModerationWSError(ctx context.Context, conn *coderws.Conn, decision *service.ContentModerationDecision) {
	if conn == nil || decision == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	message := strings.TrimSpace(decision.Message)
	if message == "" {
		message = "content moderation blocked this request"
	}
	payload, err := json.Marshal(gin.H{
		"event_id": "evt_content_moderation_blocked",
		"type":     "error",
		"error": gin.H{
			"type":    "invalid_request_error",
			"code":    contentModerationErrorCode(decision),
			"message": message,
		},
	})
	if err != nil {
		payload = []byte(`{"event_id":"evt_content_moderation_blocked","type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"content moderation blocked this request"}}`)
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

func summarizeWSCloseErrorForLog(err error) (string, string) {
	if err == nil {
		return "-", "-"
	}
	statusCode := coderws.CloseStatus(err)
	if statusCode == -1 {
		return "-", "-"
	}
	closeStatus := fmt.Sprintf("%d(%s)", int(statusCode), statusCode.String())
	closeReason := "-"
	var closeErr coderws.CloseError
	if errors.As(err, &closeErr) {
		reason := strings.TrimSpace(closeErr.Reason)
		if reason != "" {
			closeReason = reason
		}
	}
	return closeStatus, closeReason
}
