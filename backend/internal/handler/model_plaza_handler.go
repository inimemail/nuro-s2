package handler

import (
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type ModelPlazaHandler struct {
	plaza      *service.ModelPlazaService
	users      service.UserRepository
	userRates  service.UserGroupRateRepository
	settingSvc *service.SettingService
}

func NewModelPlazaHandler(plaza *service.ModelPlazaService, users service.UserRepository, userRates service.UserGroupRateRepository, settingSvc *service.SettingService) *ModelPlazaHandler {
	return &ModelPlazaHandler{plaza: plaza, users: users, userRates: userRates, settingSvc: settingSvc}
}

type modelPlazaOfficialPricing struct {
	InputPrice        *float64 `json:"input_price"`
	OutputPrice       *float64 `json:"output_price"`
	CacheWritePrice   *float64 `json:"cache_write_price"`
	CacheWrite1hPrice *float64 `json:"cache_write_1h_price,omitempty"`
	CacheReadPrice    *float64 `json:"cache_read_price"`
}

type modelPlazaModel struct {
	Name            string                     `json:"name"`
	Platform        string                     `json:"platform"`
	Pricing         *userSupportedModelPricing `json:"pricing"`
	OfficialPricing *modelPlazaOfficialPricing `json:"official_pricing"`
}

type modelPlazaGroup struct {
	ID                   int64             `json:"id"`
	Name                 string            `json:"name"`
	Description          string            `json:"description"`
	Platform             string            `json:"platform"`
	SubscriptionType     string            `json:"subscription_type"`
	RateMultiplier       float64           `json:"rate_multiplier"`
	UserRateMultiplier   *float64          `json:"user_rate_multiplier,omitempty"`
	PeakRateEnabled      bool              `json:"peak_rate_enabled"`
	PeakStart            string            `json:"peak_start"`
	PeakEnd              string            `json:"peak_end"`
	PeakRateMultiplier   float64           `json:"peak_rate_multiplier"`
	IsExclusive          bool              `json:"is_exclusive"`
	ImageRateIndependent bool              `json:"image_rate_independent"`
	ImageRateMultiplier  float64           `json:"image_rate_multiplier"`
	Models               []modelPlazaModel `json:"models"`
}

type modelPlazaResponse struct {
	Description string            `json:"description"`
	Groups      []modelPlazaGroup `json:"groups"`
}

func (h *ModelPlazaHandler) Get(c *gin.Context) {
	if h == nil || h.settingSvc == nil || h.plaza == nil {
		response.NotFound(c, "Model plaza is not enabled")
		return
	}
	runtime := h.settingSvc.GetModelPlazaRuntime(c.Request.Context())
	if !runtime.Enabled {
		response.NotFound(c, "Model plaza is not enabled")
		return
	}
	subject, authenticated := middleware.GetAuthSubjectFromContext(c)
	if runtime.RequireAuth && !authenticated {
		response.Unauthorized(c, "Authentication required")
		return
	}
	groups, err := h.plaza.List(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	var allowed map[int64]struct{}
	var rates map[int64]float64
	var restrictPublicGroups bool
	if authenticated {
		user, userErr := h.users.GetByID(c.Request.Context(), subject.UserID)
		if userErr != nil || user == nil || !user.IsActive() {
			response.Unauthorized(c, "User account is not available")
			return
		}
		allowed = make(map[int64]struct{}, len(user.AllowedGroups))
		for _, groupID := range user.AllowedGroups {
			allowed[groupID] = struct{}{}
		}
		restrictPublicGroups = user.RestrictPublicGroups
		if h.userRates != nil {
			rates, err = h.userRates.GetByUserID(c.Request.Context(), subject.UserID)
			if err != nil {
				slog.Warn("model_plaza_user_rates_failed", "error", err)
				rates = nil
			}
		}
	}
	visible := filterModelPlazaGroups(groups, allowed, authenticated, restrictPublicGroups)
	result := make([]modelPlazaGroup, 0, len(visible))
	for i := range visible {
		result = append(result, modelPlazaGroupDTO(&visible[i], rates))
	}
	response.Success(c, modelPlazaResponse{Description: runtime.Description, Groups: result})
}

func filterModelPlazaGroups(groups []service.ModelPlazaGroup, allowed map[int64]struct{}, authenticated, restrictPublicGroups bool) []service.ModelPlazaGroup {
	result := make([]service.ModelPlazaGroup, 0, len(groups))
	for i := range groups {
		group := groups[i]
		// restrict_public_groups only narrows standard public groups. Subscription
		// entitlements are resolved separately and must not be hidden by this flag.
		isRestrictedPublic := authenticated && restrictPublicGroups &&
			!group.IsExclusive && group.SubscriptionType == service.SubscriptionTypeStandard
		if group.IsExclusive || isRestrictedPublic {
			if !authenticated {
				continue
			}
			if _, ok := allowed[group.ID]; !ok {
				continue
			}
		}
		result = append(result, group)
	}
	return result
}

func modelPlazaGroupDTO(group *service.ModelPlazaGroup, rates map[int64]float64) modelPlazaGroup {
	models := make([]modelPlazaModel, 0, len(group.Models))
	for i := range group.Models {
		model := &group.Models[i]
		models = append(models, modelPlazaModel{Name: model.Name, Platform: model.Platform, Pricing: toUserPricing(model.Pricing), OfficialPricing: modelPlazaOfficialPricingDTO(model.OfficialPricing)})
	}
	result := modelPlazaGroup{
		ID: group.ID, Name: group.Name, Description: group.Description, Platform: group.Platform,
		SubscriptionType: group.SubscriptionType, RateMultiplier: group.RateMultiplier,
		PeakRateEnabled: group.PeakRateEnabled, PeakStart: group.PeakStart, PeakEnd: group.PeakEnd,
		PeakRateMultiplier: group.PeakRateMultiplier, IsExclusive: group.IsExclusive, Models: models,
		ImageRateIndependent: group.ImageRateIndependent, ImageRateMultiplier: group.ImageRateMultiplier,
	}
	if rate, ok := rates[group.ID]; ok {
		result.UserRateMultiplier = &rate
	}
	return result
}

func modelPlazaOfficialPricingDTO(pricing *service.ModelPlazaOfficialPricing) *modelPlazaOfficialPricing {
	if pricing == nil {
		return nil
	}
	return &modelPlazaOfficialPricing{InputPrice: pricing.InputPrice, OutputPrice: pricing.OutputPrice, CacheWritePrice: pricing.CacheWritePrice, CacheWrite1hPrice: pricing.CacheWrite1hPrice, CacheReadPrice: pricing.CacheReadPrice}
}
