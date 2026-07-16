package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// ChatCompletions handles OpenAI Chat Completions API requests.
// POST /v1/chat/completions
func (h *OpenAIGatewayHandler) ChatCompletions(c *gin.Context) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)

	if h.tryOpenAIEdgeIngressProxy(c) {
		return
	}

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
		"handler.openai_gateway.chat_completions",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
	)

	if !h.ensureResponsesDependencies(c, reqLog) {
		return
	}

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
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

	if !gjson.ValidBytes(body) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}

	modelResult := gjson.GetBytes(body, "model")
	if !modelResult.Exists() || modelResult.Type != gjson.String || modelResult.String() == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	if service.IsGPTImageGenerationModel(reqModel) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "GPT image models are only supported through the Images API, not Chat Completions")
		return
	}
	reqStream, ok := parseOpenAICompatibleStream(body)
	if !ok {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", invalidStreamFieldTypeMessage)
		return
	}

	reqLog = reqLog.With(zap.String("model", reqModel), zap.Bool("stream", reqStream))

	setOpsRequestContext(c, reqModel, reqStream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(reqStream, false)))

	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIChat, reqModel, body); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}

	// 解析渠道级模型映射
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)

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
		reqLog.Info("openai_chat_completions.billing_eligibility_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.handleStreamingAwareError(c, status, code, message, streamStarted)
		return
	}

	affinityBody := body
	affinityModel := reqModel
	if channelMapping.Mapped {
		affinityBody = h.gatewayService.ReplaceModelInBody(body, channelMapping.MappedModel)
		affinityModel = channelMapping.MappedModel
	}
	sessionHash := h.gatewayService.GeneratePromptCacheBoostAffinitySessionHashForGroupWithMapped(c.Request.Context(), c, apiKey.GroupID, body, reqModel, affinityBody, affinityModel)
	if sessionHash == "" {
		sessionHash = h.gatewayService.GenerateSessionHash(c, body)
	}
	promptCacheKey := h.gatewayService.ExtractSessionID(c, body)

	maxAccountSwitches := h.nonImageStreamBootstrapSwitchLimit(reqStream)
	switchCount := 0
	failedAccountIDs := make(map[int64]struct{})
	capacitySkippedIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	sameAccountRetryStartedAt := make(map[int64]time.Time)
	sameAccountRetryAccountID := int64(0)
	var sameAccountRetryAccount *service.Account
	var sameAccountRetryErr *service.UpstreamFailoverError
	var lastFailoverErr *service.UpstreamFailoverError
	modelRoutingLockedPriority := -1
	settleFailover := func(account *service.Account, failoverErr *service.UpstreamFailoverError) bool {
		sameAccountRetryAccountID = 0
		sameAccountRetryAccount = nil
		sameAccountRetryErr = nil
		if failoverClientGone(c) {
			return false
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
			h.handleFailoverExhausted(c, failoverErr, streamStarted)
			return false
		}
		switchCount++
		if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount) {
			h.handleFailoverExhausted(c, failoverErr, streamStarted)
			return false
		}
		reqLog.Warn("openai_chat_completions.upstream_failover_switching",
			zap.Int64("account_id", account.ID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("switch_count", switchCount),
			zap.Int("max_switches", maxAccountSwitches),
		)
		return true
	}

	for {
		excludedAccountIDs := mergeOpenAIAccountExclusions(failedAccountIDs, capacitySkippedIDs)
		reqLog.Debug("openai_chat_completions.account_selecting", zap.Int("excluded_account_count", len(excludedAccountIDs)))
		var (
			selection        *service.AccountSelectionResult
			scheduleDecision service.OpenAIAccountScheduleDecision
			err              error
		)
		if sameAccountRetryAccountID > 0 {
			selection, scheduleDecision, err = h.gatewayService.SelectRequiredAccountForCapabilityOnPlatformLockedPriority(
				c.Request.Context(), apiKey.GroupID, sameAccountRetryAccountID, reqModel, excludedAccountIDs,
				service.OpenAIUpstreamTransportAny, service.OpenAIEndpointCapabilityChatCompletions,
				false, requestPlatform, modelRoutingLockedPriority,
			)
		} else {
			selection, scheduleDecision, err = h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(
				c.Request.Context(),
				apiKey.GroupID,
				"",
				sessionHash,
				reqModel,
				excludedAccountIDs,
				service.OpenAIUpstreamTransportAny,
				service.OpenAIEndpointCapabilityChatCompletions,
				false,
				requestPlatform,
				modelRoutingLockedPriority,
			)
		}
		if (err != nil || selection == nil || selection.Account == nil) && sameAccountRetryAccountID > 0 {
			if failoverClientGone(c) {
				return
			}
			if sameAccountRetryAccount != nil && sameAccountRetryErr != nil {
				if !settleFailover(sameAccountRetryAccount, sameAccountRetryErr) {
					return
				}
				continue
			}
			capacitySkippedIDs[sameAccountRetryAccountID] = struct{}{}
			sameAccountRetryAccountID = 0
			continue
		}
		if err != nil {
			reqLog.Warn("openai_chat_completions.account_select_failed",
				zap.Error(openAICompatibleSelectionErrorForLog(err, requestPlatform)),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
				return
			} else {
				if lastFailoverErr != nil {
					h.handleFailoverExhausted(c, lastFailoverErr, streamStarted)
				} else {
					h.handleStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
				}
				return
			}
		}
		if selection == nil || selection.Account == nil {
			cls := classifyOpenAICompatibleNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.handleStreamingAwareError(c, cls.Status, cls.ErrType, cls.Message, streamStarted)
			return
		}
		account := selection.Account
		sessionHash = h.gatewayService.NormalizeOpenAIPromptCacheBoostAffinitySessionHash(sessionHash, account)
		sessionHash = ensureOpenAIPoolModeSessionHash(sessionHash, account)
		reqLog.Debug("openai_chat_completions.account_selected", zap.Int64("account_id", account.ID), zap.String("account_name", account.Name))
		_ = scheduleDecision
		setOpsSelectedAccount(c, account.ID, account.Platform)

		slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, reqStream, &streamStarted, reqLog)
		if !slotResult.Acquired {
			if slotResult.CapacityMiss {
				if sameAccountRetryAccountID > 0 && sameAccountRetryAccount != nil && sameAccountRetryErr != nil {
					if !settleFailover(sameAccountRetryAccount, sameAccountRetryErr) {
						return
					}
					continue
				}
				sameAccountRetryAccountID = 0
				capacitySkippedIDs[account.ID] = struct{}{}
				reqLog.Info("openai_chat_completions.account_capacity_skip",
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

		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		forwardStart := time.Now()
		markSameAccountAttemptStart(sameAccountRetryStartedAt, account, forwardStart)

		forwardBody := body
		if channelMapping.Mapped {
			forwardBody = h.gatewayService.ReplaceModelInBody(body, channelMapping.MappedModel)
		}
		writerSizeBeforeForward := c.Writer.Size()
		result, err := func() (*service.OpenAIForwardResult, error) {
			defer func() {
				if accountReleaseFunc != nil {
					accountReleaseFunc()
				}
			}()
			return h.gatewayService.ForwardAsChatCompletions(c.Request.Context(), c, account, forwardBody, promptCacheKey, "")
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
				reqLog.Warn("openai_chat_completions.forward_partial_result",
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
					if c.Writer.Size() != writerSizeBeforeForward {
						h.handleFailoverExhausted(c, failoverErr, true)
						return
					}
					// Pool mode: retry on the same account
					if failoverErr.RetryableOnSameAccount {
						retryDelay := sameAccountRetryDelayForAccount(account)
						if retryPlan, ok := planSameAccountRetry(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay); ok {
							sameAccountRetryAccountID = account.ID
							sameAccountRetryAccount = account
							sameAccountRetryErr = failoverErr
							reqLog.Warn("openai_chat_completions.pool_mode_same_account_retry",
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
					if !settleFailover(account, failoverErr) {
						return
					}
					continue
				}
				if service.GetOpsCyberPolicy(c) == nil {
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
					resolveRawCCUpstreamEndpoint(c, account),
					service.HashUsageRequestPayload(body),
					channelMapping.ToUsageFields(reqModel, ""),
				)
				reqLog.Warn("openai_chat_completions.forward_failed",
					zap.Int64("account_id", account.ID),
					zap.Bool("fallback_error_response_written", wroteFallback),
					zap.Bool("upstream_error_already_communicated", upstreamErrorAlreadyCommunicated),
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
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := resolveRawCCUpstreamEndpoint(c, account)
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
				PromptCacheAffinityHash: sessionHash,
				PromptCacheGroupID:      apiKey.GroupID,
				SkipSuccessSideEffects:  !successfulOutcome,
				APIKeyService:           h.apiKeyService,
				ChannelUsageFields:      channelMapping.ToUsageFields(reqModel, result.UpstreamModel),
				CyberBlocked:            cyberBlocked,
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.chat_completions"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai_chat_completions.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai_chat_completions.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}

// resolveRawCCUpstreamEndpoint returns the actual upstream endpoint for
// OpenAI Chat Completions requests. For APIKey accounts whose upstream
// is forced or probed to not support the Responses API, the request is
// forwarded directly to /v1/chat/completions — not through the default
// CC→Responses conversion path.
func resolveRawCCUpstreamEndpoint(c *gin.Context, account *service.Account) string {
	if account != nil && account.Type == service.AccountTypeAPIKey &&
		!openai_compat.ShouldUseResponsesAPI(account.Extra) {
		return "/v1/chat/completions"
	}
	return GetUpstreamEndpoint(c, account.Platform)
}
