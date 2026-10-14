package handler

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *GatewayHandler) GeminiCacheReconcile(c *gin.Context) {
	if h.geminiCachedContentService == nil {
		writeCacheManagementError(c, service.ErrGeminiCacheUnavailable)
		return
	}
	if c.Request.Method == http.MethodGet {
		items, next, err := h.geminiCachedContentService.ListReconcile(c.Request.Context(), c.Query("pageToken"))
		if err != nil {
			writeCacheManagementError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items, "nextPageToken": next})
		return
	}
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	var request struct {
		Reason         string `json:"reason" binding:"required,min=3,max=256"`
		ExpectedAmount string `json:"expectedAmount"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	if !ok || c.ShouldBindJSON(&request) != nil {
		googleError(c, http.StatusBadRequest, "A reconciliation reason is required")
		return
	}
	var actionErr error
	if strings.HasSuffix(c.Request.URL.Path, "/reserve-deficit") {
		actionErr = h.geminiCachedContentService.TopUpConfirmed(c.Request.Context(), c.Param("operationID"), subject.UserID, request.Reason, request.ExpectedAmount)
	} else {
		actionErr = h.geminiCachedContentService.RequeueReconcile(c.Request.Context(), c.Param("operationID"), subject.UserID, request.Reason)
	}
	if err := actionErr; err != nil {
		writeCacheManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"state": "queued"})
}

func (h *GatewayHandler) GeminiCachePanelKeys(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || h.apiKeyService == nil {
		googleError(c, http.StatusUnauthorized, "Invalid user context")
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 || page > 10000 {
		googleError(c, http.StatusBadRequest, "Invalid key page")
		return
	}
	search := strings.TrimSpace(c.Query("search"))
	if len(search) > 100 {
		googleError(c, http.StatusBadRequest, "Invalid key search")
		return
	}
	keys, pages, err := h.apiKeyService.List(c.Request.Context(), subject.UserID, pagination.PaginationParams{Page: page, PageSize: 100}, service.APIKeyListFilters{Search: search})
	if err != nil {
		googleError(c, http.StatusServiceUnavailable, "Cache keys unavailable")
		return
	}
	items := make([]gin.H, 0, len(keys))
	for _, key := range keys {
		if key.Group != nil && key.Group.Platform == service.PlatformGemini && (key.IsActive() || key.Status == service.StatusAPIKeyQuotaExhausted || key.Status == service.StatusAPIKeyExpired) {
			items = append(items, gin.H{"id": key.ID, "name": key.Name, "group": gin.H{"name": key.Group.Name}})
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "pages": pages.Pages, "page": page})
}

func (h *GatewayHandler) GeminiCachePanel(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	id, err := strconv.ParseInt(c.Param("keyID"), 10, 64)
	if !ok || err != nil || id <= 0 || h.apiKeyService == nil {
		googleError(c, http.StatusNotFound, "Cache key not found")
		return
	}
	key, err := h.apiKeyService.GetByID(c.Request.Context(), id)
	if err != nil && !errors.Is(err, service.ErrAPIKeyNotFound) {
		writeCacheManagementError(c, service.ErrGeminiCacheUnavailable)
		return
	}
	if err != nil || key == nil || key.UserID != subject.UserID {
		googleError(c, http.StatusNotFound, "Cache key not found")
		return
	}
	paid := c.Param("idempotency") == "" && (c.Request.Method == http.MethodPost || c.Request.Method == http.MethodPatch)
	if !middleware.ValidateGeminiCachePanelKey(c, key, h.cfg, paid) {
		return
	}
	sub, err := h.apiKeyService.GetActiveSubscriptionForAPIKey(c.Request.Context(), key)
	if paid && key.Group != nil && key.Group.IsSubscriptionType() && (err != nil || sub == nil) {
		googleError(c, http.StatusForbidden, "No active subscription")
		return
	}
	c.Set(string(middleware.ContextKeyAPIKey), key)
	if sub != nil {
		c.Set(string(middleware.ContextKeySubscription), sub)
	}
	if c.Param("idempotency") != "" {
		owner, ownerErr := service.GeminiCacheOwnerForKey(key)
		if ownerErr != nil || h.geminiCachedContentService == nil {
			writeCacheManagementError(c, service.ErrGeminiCacheNotFound)
			return
		}
		if c.Request.Method == http.MethodPost {
			if err := h.geminiCachedContentService.CancelUnrecordedOperation(c.Request.Context(), owner, c.Query("kind"), c.Param("idempotency")); err != nil {
				writeCacheManagementError(c, err)
				return
			}
			c.JSON(http.StatusOK, gin.H{"state": "released"})
			return
		}
		state, name, statusErr := h.geminiCachedContentService.OperationStatus(c.Request.Context(), owner, c.Query("kind"), c.Param("idempotency"))
		if statusErr != nil {
			writeCacheManagementError(c, statusErr)
			return
		}
		c.JSON(http.StatusOK, gin.H{"state": state, "resourceName": name})
		return
	}
	h.GeminiCachedContents(c)
}

func (h *GatewayHandler) GeminiCachedContents(c *gin.Context) {
	if h.geminiCachedContentService == nil || !h.geminiCachedContentService.Enabled() {
		googleError(c, http.StatusServiceUnavailable, "Explicit caching is unavailable")
		return
	}
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || middleware.HasForcePlatform(c) {
		googleError(c, http.StatusUnauthorized, "Invalid cache API key")
		return
	}
	owner, err := service.GeminiCacheOwnerForKey(key)
	if err != nil {
		googleError(c, http.StatusBadRequest, "Explicit caching requires a Gemini group")
		return
	}
	sub, _ := middleware.GetSubscriptionFromContext(c)
	ctx := c.Request.Context()
	id := c.Param("name")
	if c.Request.Method == http.MethodGet {
		if id != "" {
			resource, err := h.geminiCachedContentService.Get(ctx, owner, id)
			if err != nil {
				writeCacheManagementError(c, err)
				return
			}
			if resource == nil {
				writeCacheManagementError(c, service.ErrGeminiCacheUnavailable)
				return
			}
			if resource.State == "deleted" {
				writeCacheManagementError(c, service.ErrGeminiCacheNotFound)
				return
			}
			c.JSON(http.StatusOK, resource.PublicResponse())
			return
		}
		size := 50
		if text := c.Query("pageSize"); text != "" {
			size, err = strconv.Atoi(text)
			if err != nil || size < 1 || size > 100 {
				googleError(c, http.StatusBadRequest, "pageSize must be between 1 and 100")
				return
			}
		}
		resources, next, err := h.geminiCachedContentService.List(ctx, owner, c.Query("pageToken"), size)
		if err != nil {
			writeCacheManagementError(c, err)
			return
		}
		items := make([]map[string]any, 0, len(resources))
		for _, resource := range resources {
			items = append(items, resource.PublicResponse())
		}
		c.JSON(http.StatusOK, gin.H{"cachedContents": items, "nextPageToken": next})
		return
	}
	if c.Request.Method != http.MethodDelete && h.billingCacheService == nil {
		writeCacheManagementError(c, service.ErrGeminiCacheUnavailable)
		return
	}
	if c.Request.Method != http.MethodDelete {
		err = h.billingCacheService.CheckBillingEligibility(ctx, key.User, key, key.Group, sub, service.PlatformGemini)
	}
	if err != nil {
		googleError(c, http.StatusTooManyRequests, "Cache operation quota is unavailable")
		return
	}
	var body []byte
	if c.Request.Method != http.MethodDelete {
		// The dashboard route does not use the gateway-wide body limiter.
		// Bound raw input before allocating, including chunked requests.
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<20)
		body, err = pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
		if err != nil || len(body) > 8<<20 {
			googleError(c, http.StatusBadRequest, "Invalid cached content request")
			return
		}
		if c.Request.Method == http.MethodPost {
			subject, exists := middleware.GetAuthSubjectFromContext(c)
			if !exists {
				googleError(c, http.StatusUnauthorized, "Invalid user context")
				return
			}
			if decision := h.checkContentModeration(c, requestLogger(c, "handler.gemini_cache.create"), key, subject, service.ContentModerationProtocolGemini, "", body); decision != nil && decision.Blocked {
				googleError(c, contentModerationStatus(decision), decision.Message)
				return
			}
		}
	}
	var resource *service.GeminiCachedContent
	var op *service.GeminiCacheOperation
	if c.Request.Method == http.MethodPost {
		if c.Query("dryRun") == "true" {
			quote, quoteErr := h.geminiCachedContentService.Preview(ctx, key, sub, body)
			if quoteErr != nil {
				writeCacheManagementError(c, quoteErr)
				return
			}
			c.JSON(http.StatusOK, gin.H{"tokens": quote.Tokens, "tokenSource": quote.TokenSource, "inputCost": quote.InputCost, "storageCost": quote.StorageCost, "amount": quote.Amount, "paidUntil": quote.PaidUntil, "currency": "USD"})
			return
		}
		resource, op, err = h.geminiCachedContentService.Create(ctx, key, sub, body, c.GetHeader("Idempotency-Key"))
	} else {
		if c.Request.Method == http.MethodPatch && c.Query("dryRun") == "true" {
			quote, quoteErr := h.geminiCachedContentService.PreviewMutation(ctx, key, sub, id, body, c.Query("updateMask"))
			if quoteErr != nil {
				writeCacheManagementError(c, quoteErr)
				return
			}
			c.JSON(http.StatusOK, gin.H{"tokens": quote.Tokens, "tokenSource": quote.TokenSource, "inputCost": quote.InputCost, "storageCost": quote.StorageCost, "amount": quote.Amount, "paidUntil": quote.PaidUntil, "currency": "USD"})
			return
		}
		resource, op, err = h.geminiCachedContentService.Mutate(ctx, key, sub, id, body, c.Query("updateMask"), c.GetHeader("Idempotency-Key"), c.Request.Method == http.MethodDelete)
	}
	if op != nil {
		c.Header("X-Cache-Operation-ID", op.ID)
	}
	if err != nil {
		if op != nil && op.State != "settled" && op.State != "released" {
			err = service.ErrGeminiCacheConflict
		}
		writeCacheManagementError(c, err)
		return
	}
	if c.Request.Method == http.MethodDelete {
		c.JSON(http.StatusOK, gin.H{})
		return
	}
	c.JSON(http.StatusOK, resource.PublicResponse())
}

func writeCacheManagementError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrGeminiCacheNotFound):
		googleError(c, http.StatusNotFound, "Cached content not found")
	case errors.Is(err, service.ErrGeminiCacheConflict):
		googleError(c, http.StatusConflict, "Cache operation is pending or conflicts with an existing operation")
	case errors.Is(err, service.ErrGeminiCacheRejected):
		googleError(c, http.StatusUnprocessableEntity, "Cache operation was not executed; its reservation has been released")
	case errors.Is(err, service.ErrGeminiCacheQuota):
		googleError(c, http.StatusTooManyRequests, "Insufficient cache operation quota")
	case errors.Is(err, service.ErrGeminiCacheUnavailable), errors.Is(err, service.ErrNoAvailableAccounts):
		googleError(c, http.StatusServiceUnavailable, "Cache service temporarily unavailable")
	case errors.Is(err, service.ErrGeminiCachePermission):
		googleError(c, http.StatusNotFound, "Cached content not found")
	case errors.Is(err, service.ErrGeminiCacheInvalidRequest):
		googleError(c, http.StatusBadRequest, "Cache operation could not be completed")
	default:
		// Unknown infrastructure/provider failures do not prove that a prior
		// operation was rejected. Keep clients in the reconciliation flow.
		googleError(c, http.StatusServiceUnavailable, "Cache service temporarily unavailable")
	}
}
