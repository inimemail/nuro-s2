package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// GrokImages handles xAI image generation/editing through Grok groups.
func (h *OpenAIGatewayHandler) GrokImages(c *gin.Context) {
	endpoint := service.GrokMediaEndpointImagesGenerations
	if c != nil && c.Request != nil && c.Request.URL != nil && strings.Contains(c.Request.URL.Path, "/images/edits") {
		endpoint = service.GrokMediaEndpointImagesEdits
	}
	h.handleGrokMedia(c, endpoint, "")
}

// GrokVideoGeneration handles xAI video generation through Grok groups.
func (h *OpenAIGatewayHandler) GrokVideoGeneration(c *gin.Context) {
	h.handleGrokMedia(c, service.GrokMediaEndpointVideosGenerations, "")
}

func (h *OpenAIGatewayHandler) GrokVideoEdit(c *gin.Context) {
	h.handleGrokMedia(c, service.GrokMediaEndpointVideosEdits, "")
}

func (h *OpenAIGatewayHandler) GrokVideoExtension(c *gin.Context) {
	h.handleGrokMedia(c, service.GrokMediaEndpointVideosExtensions, "")
}

// GrokVideoStatus handles xAI video status retrieval through Grok groups.
func (h *OpenAIGatewayHandler) GrokVideoStatus(c *gin.Context) {
	h.handleGrokMedia(c, service.GrokMediaEndpointVideoStatus, c.Param("request_id"))
}

func (h *OpenAIGatewayHandler) GrokVideoContent(c *gin.Context) {
	h.handleGrokMedia(c, service.GrokMediaEndpointVideoContent, c.Param("request_id"))
}

func (h *OpenAIGatewayHandler) handleGrokMedia(c *gin.Context, endpoint service.GrokMediaEndpoint, requestID string) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

	requestStart := time.Now()
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
		"handler.openai_gateway.grok_media",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("endpoint", string(endpoint)),
	)
	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	var body []byte
	var err error
	if endpoint.RequiresRequestBody() {
		body, err = pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
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
	}

	contentType := c.GetHeader("Content-Type")
	requestInfo := service.ParseGrokMediaRequest(contentType, body)
	requestModel := requestInfo.Model
	if endpoint.IsGenerationRequest() && strings.TrimSpace(requestModel) == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if endpoint.IsVideoLookupRequest() && strings.TrimSpace(requestID) == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "request_id is required")
		return
	}

	reqLog = reqLog.With(zap.String("model", requestModel))
	setOpsRequestContext(c, requestModel, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))

	if endpoint.IsGenerationRequest() {
		if !service.GroupAllowsImageGeneration(apiKey.Group) {
			h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
			return
		}
		if moderationBody := requestInfo.ModerationBody(); len(moderationBody) > 0 {
			decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIImages, requestModel, moderationBody)
			if decision != nil && decision.Blocked {
				h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
				return
			}
		}
		imageReleaseFunc, acquired := h.acquireImageGenerationSlot(c, streamStarted)
		if !acquired {
			return
		}
		if imageReleaseFunc != nil {
			defer imageReleaseFunc()
		}
	}

	if h.errorPassthroughService != nil {
		service.BindErrorPassthroughService(c, h.errorPassthroughService)
	}

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	requestCtx := c.Request.Context()
	if err := h.billingCacheService.CheckBillingEligibility(requestCtx, apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(requestCtx, apiKey)); err != nil {
		reqLog.Info("grok_media.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}

	sessionSeed := body
	if len(sessionSeed) == 0 && strings.TrimSpace(requestID) != "" {
		sessionSeed = []byte(requestID)
	}
	sessionHash := h.gatewayService.GenerateExplicitSessionHash(c, sessionSeed)
	if endpoint.IsVideoLookupRequest() {
		sessionHash = service.GrokMediaVideoRequestSessionHash(requestID, subject.UserID, apiKey.ID)
	}

	maxAccountSwitches := h.maxAccountSwitches
	if maxAccountSwitches <= 0 {
		maxAccountSwitches = 3
	}
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	capacitySkippedIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	sameAccountRetryStartedAt := make(map[int64]time.Time)
	sameAccountRetryAccountID := int64(0)
	var sameAccountRetryAccount *service.Account
	var sameAccountRetryErr *service.UpstreamFailoverError
	sameAccountRetryExactVideoStatus := false
	var lastFailoverErr *service.UpstreamFailoverError
	modelRoutingLockedPriority := -1
	routingStart := time.Now()
	requiredCapability := grokMediaRequiredCapability(endpoint)
	settleFailover := func(account *service.Account, failoverErr *service.UpstreamFailoverError, exactVideoStatusRoute bool) bool {
		sameAccountRetryAccountID = 0
		sameAccountRetryAccount = nil
		sameAccountRetryErr = nil
		sameAccountRetryExactVideoStatus = false
		if failoverClientGone(c) {
			return false
		}
		h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, requestModel, false, nil)
		// A cache-bound status request must stay on its creating account because
		// the request ID is account-local.
		if endpoint.IsVideoLookupRequest() && exactVideoStatusRoute {
			h.handleFailoverExhausted(c, failoverErr, false)
			return false
		}
		h.gatewayService.HandleOpenAIAccountFailoverSwitch(requestCtx, apiKey.GroupID, sessionHash, account, failoverErr, requestModel)
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
			h.handleFailoverExhausted(c, failoverErr, false)
			return false
		}
		switchCount++
		reqLog.Warn("grok_media.upstream_failover_switching",
			zap.Int64("account_id", account.ID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("switch_count", switchCount),
			zap.Int("max_switches", maxAccountSwitches),
		)
		return true
	}

	for {
		excludedAccountIDs := mergeOpenAIAccountExclusions(failedAccountIDs, capacitySkippedIDs)
		var (
			selection             *service.AccountSelectionResult
			scheduleDecision      service.OpenAIAccountScheduleDecision
			exactVideoStatusRoute bool
		)
		if endpoint.IsVideoLookupRequest() {
			selection, exactVideoStatusRoute, err = h.gatewayService.SelectBoundGrokMediaVideoRequestAccount(requestCtx, apiKey.GroupID, requestID, subject.UserID, apiKey.ID)
			if errors.Is(err, service.ErrGrokMediaVideoBindingUnavailable) {
				h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Video request routing is temporarily unavailable")
				return
			}
			if errors.Is(err, service.ErrGrokMediaVideoRequestNotFound) {
				h.errorResponse(c, http.StatusNotFound, "not_found_error", "Video request not found")
				return
			}
		}
		if err == nil && !exactVideoStatusRoute {
			if sameAccountRetryAccountID > 0 {
				selection, scheduleDecision, err = h.gatewayService.SelectRequiredAccountForCapabilityOnPlatformLockedPriority(
					requestCtx, apiKey.GroupID, sameAccountRetryAccountID, requestModel, excludedAccountIDs,
					service.OpenAIUpstreamTransportHTTPSSE, requiredCapability, false, service.PlatformGrok, modelRoutingLockedPriority,
				)
			} else {
				selection, scheduleDecision, err = h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(
					requestCtx,
					apiKey.GroupID,
					"",
					sessionHash,
					requestModel,
					excludedAccountIDs,
					service.OpenAIUpstreamTransportHTTPSSE,
					requiredCapability,
					false,
					service.PlatformGrok,
					modelRoutingLockedPriority,
				)
			}
		}
		if (err != nil || selection == nil || selection.Account == nil) && sameAccountRetryAccountID > 0 {
			if failoverClientGone(c) {
				return
			}
			if sameAccountRetryAccount != nil && sameAccountRetryErr != nil {
				if !settleFailover(sameAccountRetryAccount, sameAccountRetryErr, sameAccountRetryExactVideoStatus) {
					return
				}
				continue
			}
			capacitySkippedIDs[sameAccountRetryAccountID] = struct{}{}
			sameAccountRetryAccountID = 0
			continue
		}
		if err != nil {
			reqLog.Warn("grok_media.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(excludedAccountIDs)),
			)
			if endpoint.IsGenerationRequest() && len(failedAccountIDs) == 0 && errors.Is(err, service.ErrNoAvailableAccounts) {
				markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				h.errorResponse(c, http.StatusServiceUnavailable, "grok_media_no_eligible_account", "No eligible Grok media accounts")
				return
			}
			if len(failedAccountIDs) == 0 {
				cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, requestModel, requestModel, service.PlatformGrok)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
				return
			}
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, false)
			} else {
				h.handleFailoverExhaustedSimple(c, http.StatusBadGateway, false)
			}
			return
		}
		if selection == nil || selection.Account == nil {
			if endpoint.IsGenerationRequest() {
				markOpsRoutingCapacityLimited(c)
				h.errorResponse(c, http.StatusServiceUnavailable, "grok_media_no_eligible_account", "No eligible Grok media accounts")
				return
			}
			cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, requestModel, requestModel, service.PlatformGrok)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
			return
		}

		reqLog.Debug("grok_media.account_schedule_decision",
			zap.String("layer", scheduleDecision.Layer),
			zap.Bool("sticky_session_hit", scheduleDecision.StickySessionHit),
			zap.Int("candidate_count", scheduleDecision.CandidateCount),
			zap.Int("top_k", scheduleDecision.TopK),
			zap.Int64("latency_ms", scheduleDecision.LatencyMs),
			zap.Float64("load_skew", scheduleDecision.LoadSkew),
		)

		account := selection.Account
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("grok_media.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		setOpsSelectedAccount(c, account.ID, account.Platform)

		slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, false, &streamStarted, reqLog)
		if !slotResult.Acquired {
			if slotResult.CapacityMiss {
				if sameAccountRetryAccountID > 0 && sameAccountRetryAccount != nil && sameAccountRetryErr != nil {
					if !settleFailover(sameAccountRetryAccount, sameAccountRetryErr, sameAccountRetryExactVideoStatus) {
						return
					}
					continue
				}
				if endpoint.IsVideoLookupRequest() && exactVideoStatusRoute {
					markOpsRoutingCapacityLimited(c)
					h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Video status account is busy, please retry later")
					return
				}
				capacitySkippedIDs[account.ID] = struct{}{}
				reqLog.Info("grok_media.account_capacity_skip",
					zap.Int64("account_id", account.ID),
					zap.String("reason", slotResult.Reason),
					zap.Int("capacity_skipped_count", len(capacitySkippedIDs)),
				)
				continue
			}
			return
		}
		accountReleaseFunc := slotResult.ReleaseFunc
		sameAccountRetryAccountID = 0
		sameAccountRetryAccount = nil
		sameAccountRetryErr = nil
		sameAccountRetryExactVideoStatus = false

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		markSameAccountAttemptStart(sameAccountRetryStartedAt, account, forwardStart)
		writerSizeBeforeForward := c.Writer.Size()
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.ForwardGrokMediaWithVideoBinding(
				requestCtx, c, account, endpoint, requestID, body, contentType,
				service.GrokMediaVideoBinding{GroupID: apiKey.GroupID, UserID: subject.UserID, APIKeyID: apiKey.ID},
			)
		}()

		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)

		if err != nil {
			if errors.Is(err, service.ErrGrokMediaVideoBindingUnavailable) {
				h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, requestModel, true, nil)
				h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Video request routing could not be persisted; retry later")
				reqLog.Warn("grok_media.bind_video_request_account_failed",
					zap.Int64("account_id", account.ID),
					zap.Error(err),
				)
				return
			}
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				if c.Writer.Size() != writerSizeBeforeForward {
					h.handleFailoverExhausted(c, failoverErr, true)
					return
				}
				if failoverErr.RetryableOnSameAccount {
					retryDelay := sameAccountRetryDelayForAccount(account)
					if retryPlan, ok := planSameAccountRetry(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay); ok {
						sameAccountRetryAccountID = account.ID
						sameAccountRetryAccount = account
						sameAccountRetryErr = failoverErr
						sameAccountRetryExactVideoStatus = exactVideoStatusRoute
						reqLog.Warn("grok_media.pool_mode_same_account_retry",
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
				if !settleFailover(account, failoverErr, exactVideoStatusRoute) {
					return
				}
				continue
			}
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, requestModel, false, nil)
			upstreamErrorAlreadyCommunicated := openAIForwardErrorAlreadyCommunicated(c, writerSizeBeforeForward, err)
			wroteFallback := false
			if !upstreamErrorAlreadyCommunicated {
				wroteFallback = h.ensureForwardErrorResponse(c, false)
			}
			reqLog.Warn("grok_media.forward_failed",
				zap.Int64("account_id", account.ID),
				zap.Bool("fallback_error_response_written", wroteFallback),
				zap.Bool("upstream_error_already_communicated", upstreamErrorAlreadyCommunicated),
				zap.Error(err),
			)
			return
		}

		if !endpoint.IsVideoLookupRequest() {
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, requestModel, true, nil)
		}
		if shouldRecordGrokMediaUsage(endpoint, requestModel) {
			recordGrokMediaUsage(c, h, reqLog, apiKey, subject, subscription, account, result, requestModel, body, requestID)
		}
		reqLog.Debug("grok_media.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

func grokMediaRequiredCapability(endpoint service.GrokMediaEndpoint) service.OpenAIEndpointCapability {
	if endpoint.IsGenerationRequest() {
		return service.OpenAIEndpointCapabilityGrokMediaGeneration
	}
	return ""
}

func shouldRecordGrokMediaUsage(endpoint service.GrokMediaEndpoint, requestModel string) bool {
	return endpoint.IsGenerationRequest() && strings.TrimSpace(requestModel) != ""
}

func recordGrokMediaUsage(
	c *gin.Context,
	h *OpenAIGatewayHandler,
	reqLog *zap.Logger,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	subscription *service.UserSubscription,
	account *service.Account,
	result *service.OpenAIForwardResult,
	requestModel string,
	body []byte,
	requestID string,
) {
	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	payloadForHash := body
	if len(payloadForHash) == 0 && strings.TrimSpace(requestID) != "" {
		payloadForHash = []byte(requestID)
	}
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
	quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
	channelUsageFields := service.ChannelUsageFields{
		OriginalModel:      requestModel,
		ChannelMappedModel: requestModel,
	}
	h.submitOpenAIUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:             result,
			APIKey:             apiKey,
			User:               apiKey.User,
			Account:            account,
			Subscription:       subscription,
			QuotaPlatform:      quotaPlatform,
			InboundEndpoint:    inboundEndpoint,
			UpstreamEndpoint:   upstreamEndpoint,
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			RequestPayloadHash: service.HashUsageRequestPayload(payloadForHash),
			APIKeyService:      h.apiKeyService,
			ChannelUsageFields: channelUsageFields,
		}); err != nil {
			logger.L().With(
				zap.String("component", "handler.openai_gateway.grok_media"),
				zap.Int64("user_id", subject.UserID),
				zap.Int64("api_key_id", apiKey.ID),
				zap.Any("group_id", apiKey.GroupID),
				zap.String("model", requestModel),
				zap.Int64("account_id", account.ID),
			).Error("grok_media.record_usage_failed", zap.Error(err))
			reqLog.Debug("grok_media.record_usage_failed", zap.Error(err))
		}
	})
}
