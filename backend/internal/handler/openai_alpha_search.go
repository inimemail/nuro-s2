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
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

func (h *OpenAIGatewayHandler) AlphaSearch(c *gin.Context) {
	streamStarted := false
	defer h.recoverResponsesPanic(c, &streamStarted)
	setOpenAIClientTransportHTTP(c)
	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey.Group == nil {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if apiKey.Group.Platform != service.PlatformOpenAI {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Alpha search is not available for this group")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	reqLog := requestLogger(c, "handler.openai_gateway.alpha_search",
		zap.Int64("user_id", subject.UserID), zap.Int64("api_key_id", apiKey.ID), zap.Any("group_id", apiKey.GroupID))
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
	if len(body) == 0 || !gjson.ValidBytes(body) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	modelResult := gjson.GetBytes(body, "model")
	if modelResult.Type != gjson.String || strings.TrimSpace(modelResult.String()) == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	requestedModel := strings.TrimSpace(modelResult.String())
	setOpsRequestContext(c, requestedModel, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))
	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, requestedModel)
	forwardBody := openAIModelMappedBody(body, channelMapping.Mapped, channelMapping.MappedModel, h.gatewayService.ReplaceModelInBody)
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())

	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}

	// Alpha Search must not read or write the sticky/cache-affinity state shared
	// by normal Responses and Chat traffic.
	const sessionHash = ""
	failedAccountIDs := make(map[int64]struct{})
	capacitySkippedIDs := make(map[int64]struct{})
	ineligibleAccountIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	sameAccountRetryStartedAt := make(map[int64]time.Time)
	sameAccountRetryAccountID := int64(0)
	var sameAccountRetryAccount *service.Account
	var sameAccountRetryErr *service.UpstreamFailoverError
	var lastFailoverErr *service.UpstreamFailoverError
	switchCount := 0
	modelRoutingLockedPriority := -1
	routingStart := time.Now()
	settleFailover := func(account *service.Account, failoverErr *service.UpstreamFailoverError) bool {
		sameAccountRetryAccountID = 0
		sameAccountRetryAccount = nil
		sameAccountRetryErr = nil
		modelRoutingLockedPriority = lockOpenAIModelRoutingFailoverPriority(
			modelRoutingLockedPriority,
			account,
			failoverErr,
			h.gatewayService == nil || h.gatewayService.IsOpenAIPoolDownstreamModelLimitProtectionEnabled(c.Request.Context()),
		)
		// Alpha Search remains request-local: do not alter shared health,
		// cooldown, prompt-cache, or sticky-session state.
		failedAccountIDs[account.ID] = struct{}{}
		lastFailoverErr = failoverErr
		if switchCount >= h.maxAccountSwitches {
			h.handleFailoverExhausted(c, failoverErr, false)
			return false
		}
		switchCount++
		if h.gatewayService.ShouldStopOpenAIOAuth429Failover(account, failoverErr.StatusCode, switchCount) {
			h.handleFailoverExhausted(c, failoverErr, false)
			return false
		}
		reqLog.Warn("openai_alpha_search.upstream_failover_switching",
			zap.Int64("account_id", account.ID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("switch_count", switchCount),
		)
		return true
	}
	for {
		excluded := mergeOpenAIAccountExclusions(failedAccountIDs, capacitySkippedIDs, ineligibleAccountIDs)
		var (
			selection *service.AccountSelectionResult
			err       error
		)
		if sameAccountRetryAccountID > 0 {
			selection, _, err = h.gatewayService.SelectRequiredAccountForCapabilityOnPlatformLockedPriority(
				c.Request.Context(), apiKey.GroupID, sameAccountRetryAccountID, requestedModel, excluded,
				service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityAlphaSearch,
				false, service.PlatformOpenAI, modelRoutingLockedPriority,
			)
		} else {
			selection, _, err = h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(
				c.Request.Context(),
				apiKey.GroupID,
				"",
				sessionHash,
				requestedModel,
				excluded,
				service.OpenAIUpstreamTransportHTTPSSE,
				service.OpenAIEndpointCapabilityAlphaSearch,
				false,
				service.PlatformOpenAI,
				modelRoutingLockedPriority,
			)
		}
		if (err != nil || selection == nil || selection.Account == nil) && sameAccountRetryAccountID > 0 {
			if c.Request.Context().Err() != nil {
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
		if err != nil || selection == nil || selection.Account == nil {
			if len(failedAccountIDs) == 0 {
				cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, requestedModel, requestedModel, service.PlatformOpenAI)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
			} else if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, false)
			} else {
				h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
			}
			return
		}
		account := selection.Account
		if !service.IsOpenAIAlphaSearchAccountEligible(account) {
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if selection.UserReleaseFunc != nil {
				selection.UserReleaseFunc()
			}
			ineligibleAccountIDs[account.ID] = struct{}{}
			continue
		}
		setOpsSelectedAccount(c, account.ID, account.Platform)
		slot := h.acquireResponsesAccountSlot(c, apiKey.GroupID, sessionHash, selection, false, &streamStarted, reqLog)
		if !slot.Acquired {
			if slot.CapacityMiss {
				if sameAccountRetryAccountID > 0 && sameAccountRetryAccount != nil && sameAccountRetryErr != nil {
					if !settleFailover(sameAccountRetryAccount, sameAccountRetryErr) {
						return
					}
					continue
				}
				sameAccountRetryAccountID = 0
				capacitySkippedIDs[account.ID] = struct{}{}
				continue
			}
			return
		}
		sameAccountRetryAccountID = 0
		sameAccountRetryAccount = nil
		sameAccountRetryErr = nil
		service.SetOpsLatencyMs(c, service.OpsRoutingLatencyMsKey, time.Since(routingStart).Milliseconds())
		writerSize := c.Writer.Size()
		forwardStart := time.Now()
		markSameAccountAttemptStart(sameAccountRetryStartedAt, account, forwardStart)
		result, forwardErr := func() (*service.OpenAIForwardResult, error) {
			if slot.ReleaseFunc != nil {
				defer slot.ReleaseFunc()
			}
			return h.gatewayService.ForwardAlphaSearch(c.Request.Context(), c, account, forwardBody)
		}()
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, time.Since(forwardStart).Milliseconds())
		if forwardErr == nil {
			if result != nil {
				h.recordAlphaSearchUsage(c, apiKey, account, subscription, channelMapping, requestedModel, body, result, subject.UserID)
			}
			return
		}
		var failoverErr *service.UpstreamFailoverError
		if !errors.As(forwardErr, &failoverErr) {
			if c.Writer.Size() == writerSize {
				h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
			}
			reqLog.Warn("openai_alpha_search.forward_failed", zap.Int64("account_id", account.ID), zap.Error(forwardErr))
			return
		}
		if c.Writer.Size() != writerSize {
			h.handleFailoverExhausted(c, failoverErr, true)
			return
		}
		if failoverErr.RetryableOnSameAccount {
			retryDelay := sameAccountRetryDelayForAccount(account)
			if retryPlan, retry := planSameAccountRetry(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay); retry {
				sameAccountRetryAccountID = account.ID
				sameAccountRetryAccount = account
				sameAccountRetryErr = failoverErr
				reqLog.Warn("openai_alpha_search.pool_mode_same_account_retry",
					zap.Int64("account_id", account.ID),
					zap.Int("upstream_status", failoverErr.StatusCode),
					zap.Int("retry_limit", retryPlan.RetryLimit),
					zap.Int("retry_count", retryPlan.RetryCount),
					zap.Duration("retry_delay", retryPlan.Delay),
					zap.Duration("retry_elapsed", retryPlan.Elapsed),
					zap.Duration("retry_max_elapsed", retryPlan.MaxElapsed),
				)
				if !sleepWithContext(c.Request.Context(), retryPlan.Delay) {
					return
				}
				continue
			}
		}
		if !settleFailover(account, failoverErr) {
			return
		}
	}
}

func (h *OpenAIGatewayHandler) recordAlphaSearchUsage(c *gin.Context, apiKey *service.APIKey, account *service.Account, subscription *service.UserSubscription, channelMapping service.ChannelMappingResult, requestedModel string, body []byte, result *service.OpenAIForwardResult, userID int64) {
	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	requestPayloadHash := service.HashUsageRequestPayload(body)
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
	quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
	h.submitMandatoryUsageRecordTask(c.Request.Context(), func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result: result, APIKey: apiKey, User: apiKey.User, Account: account, Subscription: subscription,
			InboundEndpoint: inboundEndpoint, UpstreamEndpoint: upstreamEndpoint, UserAgent: userAgent,
			IPAddress: clientIP, RequestPayloadHash: requestPayloadHash, APIKeyService: h.apiKeyService,
			QuotaPlatform:          quotaPlatform,
			SkipSuccessSideEffects: true,
			ChannelUsageFields:     channelMapping.ToUsageFields(requestedModel, result.UpstreamModel),
		}); err != nil {
			logger.L().Error("openai_alpha_search.record_usage_failed", zap.Int64("user_id", userID), zap.Error(err))
		}
	})
}
