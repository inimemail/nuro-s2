package handler

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/typesafe"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// SystemOne handles the native TypeSafe System One API. It is deliberately
// separate from Messages/Responses/Chat Completions to preserve protocol
// isolation and avoid applying incompatible request transformations.
func (h *GatewayHandler) SystemOne(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if apiKey.Group == nil || apiKey.Group.Platform != service.PlatformTypeSafe {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "System One is not enabled for this group")
		return
	}
	body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}
	model, err := typesafe.ValidateSystemOneRequest(body)
	if err != nil {
		status := http.StatusBadRequest
		if err == typesafe.ErrStreamingUnsupported {
			status = http.StatusBadRequest
		}
		h.errorResponse(c, status, "invalid_request_error", err.Error())
		return
	}
	if model != typesafe.JevLatestModel {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model must be "+typesafe.JevLatestModel)
		return
	}
	if err := h.gatewayService.ValidateModelPricing(c.Request.Context(), apiKey, model); err != nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "billing_error", "TypeSafe model pricing is not configured")
		return
	}

	// System One is a non-streaming generation endpoint, but it still shares
	// the same user concurrency and billing gates as the other generation APIs.
	// Keep these checks after request validation so malformed payloads do not
	// consume a user slot or trigger a balance lookup.
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	maxWait := service.CalculateMaxWaitWithExtra(subject.Concurrency, h.concurrencyHelper.userWaitingExtra())
	canWait, err := h.concurrencyHelper.IncrementWaitCount(c.Request.Context(), subject.UserID, maxWait)
	waitCounted := false
	if err != nil {
		// Preserve the existing fail-open behavior when the wait counter backend
		// is unavailable; the actual slot acquisition still enforces capacity.
	} else if !canWait {
		h.concurrencyHelper.setRetryAfter(c)
		h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "Too many pending requests, please retry later")
		return
	} else {
		waitCounted = true
	}
	defer func() {
		if waitCounted {
			h.concurrencyHelper.DecrementWaitCount(c.Request.Context(), subject.UserID)
		}
	}()

	streamStarted := false
	userRelease, err := h.concurrencyHelper.AcquireUserSlotWithWait(c, subject.UserID, subject.Concurrency, false, &streamStarted)
	if err != nil {
		h.handleConcurrencyError(c, err, "user", streamStarted)
		return
	}
	if waitCounted {
		h.concurrencyHelper.DecrementWaitCount(c.Request.Context(), subject.UserID)
		waitCounted = false
	}
	userRelease = wrapReleaseOnDone(c.Request.Context(), userRelease)
	if userRelease != nil {
		defer userRelease()
	}

	if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
		statusCode, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, statusCode, code, message)
		return
	}

	selection, err := h.gatewayService.SelectAccountWithLoadAwareness(c.Request.Context(), apiKey.GroupID, "", model, nil, "", apiKey.UserID)
	if err != nil || selection == nil || selection.Account == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "overloaded_error", "No available TypeSafe account")
		return
	}
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	account := selection.Account
	if !account.IsTypeSafe() {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "No TypeSafe account is available")
		return
	}
	result, responseBody, status, err := h.gatewayService.ForwardSystemOne(c.Request.Context(), account, body)
	if err != nil {
		// Keep adaptive scheduling health in sync with every completed upstream
		// attempt. TypeSafe is non-streaming, so there is no first-token sample.
		h.reportAccountScheduleResult(account, false, nil)
		if status < 400 {
			status = http.StatusBadGateway
		}
		h.errorResponse(c, status, "upstream_error", err.Error())
		return
	}
	c.Header("Content-Type", "application/json")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/json", responseBody)
	if result == nil {
		h.reportAccountScheduleResult(account, false, nil)
		return
	}
	h.reportAccountScheduleResult(account, true, nil)
	parsed := &service.ParsedRequest{Body: body, Model: model, GroupID: apiKey.GroupID}
	quotaPlatform := service.QuotaPlatform(c.Request.Context(), apiKey)
	h.submitUsageRecordTask(c.Request.Context(), func(ctx context.Context) {
		_ = h.gatewayService.RecordUsage(ctx, &service.RecordUsageInput{
			Result: result, ParsedRequest: parsed, APIKey: apiKey, User: apiKey.User, Account: account,
			InboundEndpoint: EndpointSystemOne, UpstreamEndpoint: EndpointSystemOne,
			UserAgent: c.GetHeader("User-Agent"), IPAddress: ip.GetClientIP(c),
			RequestPayloadHash: service.HashUsageRequestPayload(body), APIKeyService: h.apiKeyService,
			QuotaPlatform: quotaPlatform,
		})
	})
}
