package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// Seedance preserves the provider's native task protocol. It never enters the
// text Edge path and does not reuse Grok's Redis-only video ownership bindings.
func (h *OpenAIGatewayHandler) Seedance(c *gin.Context) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return
	}
	if key.Group == nil || key.Group.Platform != service.PlatformOpenAI {
		h.errorResponse(c, 400, "invalid_request_error", "Seedance requires an OpenAI group")
		return
	}
	if h.seedance == nil {
		h.errorResponse(c, 503, "api_error", "Seedance is unavailable")
		return
	}
	if c.Request.Method != http.MethodPost {
		h.seedanceLookup(c, key)
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, 401, "authentication_error", "User context unavailable")
		return
	}
	log := requestLogger(c, "handler.seedance", zap.Int64("api_key_id", key.ID))
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, 413, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
		} else {
			h.errorResponse(c, 400, "invalid_request_error", "Invalid request body")
		}
		return
	}
	info, err := service.ParseSeedanceRequest(body)
	if err != nil {
		h.errorResponse(c, 400, "invalid_request_error", err.Error())
		return
	}
	operationID, err := service.SeedanceIdempotencyID(key, c.GetHeader("Idempotency-Key"))
	if err != nil {
		h.errorResponse(c, 400, "invalid_request_error", err.Error())
		return
	}
	if operationID != "" && h.seedanceReplay(c, key, operationID, body) {
		return
	}
	setOpsRequestContext(c, info.Model, false)
	setOpsEndpointContext(c, "/api/v3/contents/generations/tasks", int16(service.RequestTypeSync))
	if !service.GroupAllowsImageGenerationForRequest(c.Request.Context(), key.Group, "seedance_create", info.Model, body) {
		h.errorResponse(c, 403, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}
	if decision := h.checkContentModeration(c, log, key, subject, service.ContentModerationProtocolOpenAIImages, info.Model, info.ModerationBody()); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}
	mapping, restricted := h.gatewayService.ResolveChannelMappingAndRestrict(c.Request.Context(), key.GroupID, info.Model)
	if restricted {
		h.errorResponse(c, 403, "permission_error", "Model is not available in this group")
		return
	}
	routing := info.Model
	if mapping.Mapped {
		routing = mapping.MappedModel
	}
	sub, _ := middleware.GetSubscriptionFromContext(c)
	if err = h.billingCacheService.CheckBillingEligibility(c.Request.Context(), key.User, key, key.Group, sub, service.QuotaPlatform(c.Request.Context(), key)); err != nil {
		status, code, message, _ := billingErrorDetails(err)
		h.errorResponse(c, status, code, message)
		return
	}
	if c.Request.Context().Err() != nil {
		return
	}
	// Keep the HTTP permits alive while an accepted create is persisted even
	// if the downstream disconnects. This context has a strict bounded lifetime.
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 2*time.Minute)
	defer cancel()
	c.Request = c.Request.WithContext(createCtx)
	started := false
	userRelease, acquired := h.acquireResponsesUserSlot(c, subject.UserID, subject.Concurrency, false, &started, log)
	if !acquired {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	mediaRelease, acquired := h.acquireImageGenerationSlot(c, false)
	if !acquired {
		return
	}
	if mediaRelease != nil {
		defer mediaRelease()
	}
	excluded := map[int64]struct{}{}
	for len(excluded) < 32 {
		selection, _, selectErr := h.gatewayService.SelectAccountWithSchedulerForCapabilityOnPlatformLockedPriority(c.Request.Context(), key.GroupID, "", "", routing, excluded, service.OpenAIUpstreamTransportHTTPSSE, service.OpenAIEndpointCapabilitySeedance, false, service.PlatformOpenAI, -1)
		if selectErr != nil || selection == nil || selection.Account == nil {
			h.errorResponse(c, 503, "seedance_no_eligible_account", "No eligible Seedance accounts")
			return
		}
		slot := h.acquireResponsesAccountSlot(c, key.GroupID, "", selection, false, &started, log)
		if !slot.Acquired {
			if slot.CapacityMiss {
				excluded[selection.Account.ID] = struct{}{}
				continue
			}
			return
		}
		a := selection.Account
		billingModel := seedanceBillingModel(mapping, info.Model, routing, a.GetMappedModel(routing))
		task, prepareErr := h.seedance.Prepare(c.Request.Context(), a, key, sub, info.Model, routing, billingModel, c.Request.URL.Path, operationID, body)
		if prepareErr != nil {
			if slot.ReleaseFunc != nil {
				slot.ReleaseFunc()
			}
			if operationID != "" && (errors.Is(prepareErr, service.ErrSeedanceDuplicate) || errors.Is(prepareErr, service.ErrSeedanceCapacity)) {
				if h.seedanceReplay(c, key, operationID, body) {
					return
				}
			}
			if errors.Is(prepareErr, service.ErrSeedanceCapacity) {
				excluded[a.ID] = struct{}{}
				continue
			}
			if errors.Is(prepareErr, service.ErrSeedancePricing) {
				h.errorResponse(c, 400, "seedance_pricing_required", prepareErr.Error())
			} else {
				log.Error("seedance.prepare_failed", zap.Error(prepareErr))
				h.errorResponse(c, 503, "api_error", "Unable to persist Seedance task")
			}
			return
		}
		// Once reserved, do not select another account or retry the POST.
		if slot.ReleaseFunc != nil {
			defer slot.ReleaseFunc()
		}
		setOpsSelectedAccount(c, a.ID, a.Platform)
		c.Header("X-Seedance-Operation-ID", task.ID)
		requestAt := time.Now()
		status, data, createErr := h.seedance.Create(c.Request.Context(), a, task, body)
		service.SetOpsLatencyMs(c, service.OpsUpstreamLatencyMsKey, time.Since(requestAt).Milliseconds())
		if createErr != nil {
			log.Error("seedance.create_unknown", zap.String("operation_id", task.ID), zap.Error(createErr))
			h.errorResponse(c, 502, "seedance_submission_unknown", "Submission outcome is unknown; do not automatically resubmit. Query using X-Seedance-Operation-ID.")
			return
		}
		if status < 200 || status >= 300 {
			h.errorResponse(c, status, "upstream_error", "Seedance upstream rejected the request")
			return
		}
		c.Data(status, "application/json", data)
		return
	}
	h.errorResponse(c, 429, "seedance_capacity", "Seedance task capacity reached; retry later")
}

func seedanceBillingModel(mapping service.ChannelMappingResult, requested, routed, upstream string) string {
	switch mapping.BillingModelSource {
	case service.BillingModelSourceChannelMapped:
		return routed
	case service.BillingModelSourceUpstream:
		return upstream
	default:
		return requested
	}
}

// Returns true once a response is written. An unavailable ownership store must
// fail closed; otherwise an idempotent retry could create another paid task.
func (h *OpenAIGatewayHandler) seedanceReplay(c *gin.Context, key *service.APIKey, id string, body []byte) bool {
	task, err := h.seedance.Owned(c.Request.Context(), id, key)
	if errors.Is(err, service.ErrSeedanceTaskNotFound) {
		return false
	}
	if err != nil {
		h.errorResponse(c, 503, "api_error", "Seedance idempotency lookup unavailable")
		return true
	}
	c.Header("X-Seedance-Operation-ID", task.ID)
	if task.Snapshot.PayloadHash != service.HashUsageRequestPayload(body) {
		h.errorResponse(c, 409, "idempotency_conflict", "Idempotency-Key was used with a different request")
	} else if task.ProviderID != "" {
		c.JSON(200, gin.H{"id": task.ProviderID})
	} else {
		h.errorResponse(c, 409, "seedance_submission_unconfirmed", "This operation already exists; query its operation ID instead of resubmitting")
	}
	return true
}

func (h *OpenAIGatewayHandler) seedanceLookup(c *gin.Context, key *service.APIKey) {
	id := strings.TrimSpace(c.Param("task_id"))
	if id == "" {
		h.errorResponse(c, 400, "invalid_request_error", "task_id is required")
		return
	}
	task, err := h.seedance.Owned(c.Request.Context(), id, key)
	if err != nil {
		if errors.Is(err, service.ErrSeedanceTaskNotFound) {
			h.errorResponse(c, 404, "not_found_error", "Seedance task not found")
		} else if errors.Is(err, service.ErrSeedanceAmbiguousID) {
			h.errorResponse(c, 409, "seedance_ambiguous_task", "Provider task ID is ambiguous; query using X-Seedance-Operation-ID")
		} else {
			h.errorResponse(c, 503, "api_error", "Seedance task lookup unavailable")
		}
		return
	}
	setOpsRequestContext(c, task.Snapshot.Model, false)
	setOpsSelectedAccount(c, task.AccountID, service.PlatformOpenAI)
	c.Header("X-Seedance-Operation-ID", task.ID)
	if c.Request.Method == http.MethodDelete {
		status, data, err := h.seedance.Delete(c.Request.Context(), task)
		if err != nil || status < 200 || status >= 300 {
			if status < 400 || status > 599 {
				status = 502
			}
			h.errorResponse(c, status, "upstream_error", "Seedance cancellation could not be confirmed; the task remains available for reconciliation")
			return
		}
		c.Data(status, "application/json", data)
		return
	}
	if task.ProviderID == "" {
		if task.State == "rejected" {
			c.JSON(http.StatusOK, gin.H{"id": task.ID, "status": task.State, "error": gin.H{"code": "submission_rejected", "message": "The upstream rejected this submission"}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": task.ID, "status": task.State, "error": gin.H{"code": "submission_unconfirmed", "message": "Provider acceptance is unconfirmed; automatic resubmission is unsafe"}})
		return
	}
	if len(task.Response) > 0 {
		body := []byte(task.Response)
		if !gjson.GetBytes(body, "status").Exists() {
			body, _ = sjson.SetBytes(body, "status", task.State)
		}
		if upstreamError := gjson.GetBytes(body, "error"); upstreamError.Exists() && upstreamError.Type != gjson.Null {
			body, _ = sjson.SetBytes(body, "error", gin.H{"code": "generation_failed", "message": "Video generation did not complete successfully"})
		}
		c.Data(200, "application/json", body)
		return
	}
	c.JSON(200, gin.H{"id": task.ProviderID, "status": task.State})
}
