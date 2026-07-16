package handler

import (
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

type keyBillingInfoResponse struct {
	Object                  string    `json:"object"`
	SchemaVersion           int       `json:"schema_version"`
	BillingScope            string    `json:"billing_scope"`
	GroupRateMultiplier     float64   `json:"group_rate_multiplier"`
	UserRateMultiplier      *float64  `json:"user_rate_multiplier,omitempty"`
	ResolvedRateMultiplier  float64   `json:"resolved_rate_multiplier"`
	PeakRateEnabled         bool      `json:"peak_rate_enabled"`
	PeakStart               *string   `json:"peak_start,omitempty"`
	PeakEnd                 *string   `json:"peak_end,omitempty"`
	PeakRateMultiplier      *float64  `json:"peak_rate_multiplier,omitempty"`
	AppliedPeakMultiplier   *float64  `json:"applied_peak_multiplier,omitempty"`
	EffectiveRateMultiplier float64   `json:"effective_rate_multiplier"`
	Timezone                *string   `json:"timezone,omitempty"`
	ObservedAt              time.Time `json:"observed_at"`
}

// KeyBillingInfo exposes billing configuration for observation and upstream
// probes. It never changes routing or scheduler weights.
func (h *GatewayHandler) KeyBillingInfo(c *gin.Context) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok || apiKey == nil {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if h.cfg != nil && h.cfg.RunMode == config.RunModeSimple {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Billing information is not supported in simple mode")
		return
	}
	if apiKey.GroupID == nil || apiKey.Group == nil {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "API key is not assigned to a group")
		return
	}
	resolved := apiKey.Group.RateMultiplier
	if h.gatewayService != nil {
		resolved = h.gatewayService.ResolveUserGroupRateMultiplier(c.Request.Context(), apiKey.UserID, *apiKey.GroupID, resolved)
	}
	now := timezone.Now()
	appliedPeak := apiKey.Group.PeakMultiplierAt(now)
	result := keyBillingInfoResponse{
		Object: "sub2api.key_billing", SchemaVersion: 1, BillingScope: "token",
		GroupRateMultiplier: apiKey.Group.RateMultiplier, ResolvedRateMultiplier: resolved,
		PeakRateEnabled: apiKey.Group.PeakRateEnabled, EffectiveRateMultiplier: resolved * appliedPeak,
		ObservedAt: now.UTC(),
	}
	if resolved != apiKey.Group.RateMultiplier {
		userRate := resolved
		result.UserRateMultiplier = &userRate
	}
	if apiKey.Group.PeakRateEnabled {
		start, end, multiplier, applied := apiKey.Group.PeakStart, apiKey.Group.PeakEnd, apiKey.Group.PeakRateMultiplier, appliedPeak
		result.PeakStart, result.PeakEnd = &start, &end
		result.PeakRateMultiplier, result.AppliedPeakMultiplier = &multiplier, &applied
		tz := timezone.Location().String()
		result.Timezone = &tz
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, result)
}
