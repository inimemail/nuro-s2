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

// Embeddings handles the OpenAI-compatible Embeddings API.
// POST /v1/embeddings
func (h *OpenAIGatewayHandler) Embeddings(c *gin.Context) {
	streamStarted := false
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
		"handler.openai_gateway.embeddings",
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
	if !modelResult.Exists() || modelResult.Type != gjson.String || strings.TrimSpace(modelResult.String()) == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	reqModel := modelResult.String()
	reqLog = reqLog.With(zap.String("model", reqModel))
	setOpsRequestContext(c, reqModel, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeSync))

	channelMapping, _ := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), apiKey.GroupID, reqModel)

	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(requestStart).Milliseconds())

	userReleaseFunc, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &streamStarted, reqLog)
	if !acquired {
		return
	}
	if userReleaseFunc != nil {
		defer userReleaseFunc()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		reqLog.Info("openai_embeddings.billing_check_failed", zap.Error(err))
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}

	failedAccountIDs := make(map[int64]struct{})
	capacitySkippedIDs := make(map[int64]struct{})
	sameAccountRetryCount := make(map[int64]int)
	sameAccountRetryStartedAt := make(map[int64]time.Time)
	var lastFailoverErr *service.UpstreamFailoverError
	retryAccountID := int64(0)
	var retryAccount *service.Account
	var retryFailoverErr *service.UpstreamFailoverError
	modelRoutingLockedPriority := -1
	switchCount := 0
	maxAccountSwitches := h.maxAccountSwitches
	if maxAccountSwitches <= 0 {
		maxAccountSwitches = 3
	}
	routingStart := time.Now()
	settleFailover := func(account *service.Account, failoverErr *service.UpstreamFailoverError) bool {
		retryAccountID = 0
		retryAccount = nil
		retryFailoverErr = nil
		if failoverClientGone(c) {
			return false
		}
		h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
		h.gatewayService.HandleOpenAIAccountFailoverSwitch(c.Request.Context(), apiKey.GroupID, "", account, failoverErr)
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
		reqLog.Warn("openai_embeddings.upstream_failover_switching",
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
			selection *service.AccountSelectionResult
			err       error
		)
		if retryAccountID > 0 {
			selection, _, err = h.gatewayService.SelectRequiredAccountForCapabilityOnPlatformLockedPriority(
				c.Request.Context(), apiKey.GroupID, retryAccountID, reqModel, excludedAccountIDs,
				service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityEmbeddings,
				false, service.PlatformOpenAI, modelRoutingLockedPriority,
			)
		} else {
			selection, _, err = h.gatewayService.SelectAccountWithSchedulerForCapabilityAndStickyAccountLockedPriority(
				c.Request.Context(), apiKey.GroupID, "", "", 0, reqModel, excludedAccountIDs,
				service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilityEmbeddings,
				false, modelRoutingLockedPriority,
			)
		}
		if (err != nil || selection == nil || selection.Account == nil) && retryAccountID > 0 {
			if failoverClientGone(c) {
				return
			}
			if retryAccount != nil && retryFailoverErr != nil {
				if !settleFailover(retryAccount, retryFailoverErr) {
					return
				}
				continue
			}
			capacitySkippedIDs[retryAccountID] = struct{}{}
			retryAccountID = 0
			continue
		}
		if err != nil {
			reqLog.Warn("openai_embeddings.account_select_failed",
				zap.Error(err),
				zap.Int("excluded_account_count", len(failedAccountIDs)),
			)
			if len(failedAccountIDs) == 0 {
				cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, service.PlatformOpenAI)
				if !cls.ModelNotFound {
					markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
				}
				h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
				return
			}
			if lastFailoverErr != nil {
				h.handleFailoverExhausted(c, lastFailoverErr, false)
			} else {
				h.errorResponse(c, http.StatusBadGateway, "api_error", "Upstream request failed")
			}
			return
		}
		if selection == nil || selection.Account == nil {
			cls := classifyNoAccountErrorFromGin(c, h.gatewayService, apiKey, reqModel, reqModel, service.PlatformOpenAI)
			if !cls.ModelNotFound {
				markOpsRoutingCapacityLimited(c)
			}
			h.errorResponse(c, cls.Status, cls.ErrType, cls.Message)
			return
		}
		account := selection.Account
		setOpsSelectedAccount(c, account.ID, account.Platform)

		slotResult := h.acquireResponsesAccountSlot(c, apiKey.GroupID, "", selection, false, &streamStarted, reqLog)
		if !slotResult.Acquired {
			if slotResult.CapacityMiss {
				if retryAccountID > 0 && retryAccount != nil && retryFailoverErr != nil {
					if !settleFailover(retryAccount, retryFailoverErr) {
						return
					}
					continue
				}
				retryAccountID = 0
				capacitySkippedIDs[account.ID] = struct{}{}
				reqLog.Info("openai_embeddings.account_capacity_skip",
					zap.Int64("account_id", account.ID),
					zap.String("reason", slotResult.Reason),
					zap.Int("capacity_skipped_count", len(capacitySkippedIDs)),
				)
				continue
			}
			return
		}
		accountReleaseFunc := slotResult.ReleaseFunc
		retryAccountID = 0
		retryAccount = nil
		retryFailoverErr = nil

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
			return h.gatewayService.ForwardEmbeddings(c.Request.Context(), c, account, forwardBody, "")
		}()

		forwardDurationMs := time.Since(forwardStart).Milliseconds()
		upstreamLatencyMs, _ := getContextInt64(c, service.OpsUpstreamLatencyMsKey)
		responseLatencyMs := forwardDurationMs
		if upstreamLatencyMs > 0 && forwardDurationMs > upstreamLatencyMs {
			responseLatencyMs = forwardDurationMs - upstreamLatencyMs
		}
		service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, responseLatencyMs)

		if err != nil {
			var failoverErr *service.UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				if c.Writer.Size() != writerSizeBeforeForward {
					h.handleFailoverExhausted(c, failoverErr, true)
					return
				}
				if failoverErr.RetryableOnSameAccount {
					retryDelay := sameAccountRetryDelayForAccount(account)
					if retryPlan, ok := planSameAccountRetry(account, sameAccountRetryCount, sameAccountRetryStartedAt, retryDelay); ok {
						retryAccountID = account.ID
						retryAccount = account
						retryFailoverErr = failoverErr
						reqLog.Warn("openai_embeddings.pool_mode_same_account_retry",
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
			h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, false, nil)
			if c.Writer.Size() == writerSizeBeforeForward {
				h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
			}
			reqLog.Warn("openai_embeddings.forward_failed",
				zap.Int64("account_id", account.ID),
				zap.Error(err),
			)
			return
		}

		h.gatewayService.ReportOpenAIAccountScheduleResultForRequest(account, reqModel, true, nil)
		userAgent := c.GetHeader("User-Agent")
		clientIP := ip.GetClientIP(c)
		inboundEndpoint := GetInboundEndpoint(c)
		upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
		quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)

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
				APIKeyService:      h.apiKeyService,
				ChannelUsageFields: channelMapping.ToUsageFields(reqModel, result.UpstreamModel),
			}); err != nil {
				logger.L().With(
					zap.String("component", "handler.openai_gateway.embeddings"),
					zap.Int64("user_id", subject.UserID),
					zap.Int64("api_key_id", apiKey.ID),
					zap.Any("group_id", apiKey.GroupID),
					zap.String("model", reqModel),
					zap.Int64("account_id", account.ID),
				).Error("openai_embeddings.record_usage_failed", zap.Error(err))
			}
		})
		reqLog.Debug("openai_embeddings.request_completed",
			zap.Int64("account_id", account.ID),
			zap.Int("switch_count", switchCount),
		)
		return
	}
}
