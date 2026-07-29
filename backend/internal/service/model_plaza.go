package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const modelPlazaSnapshotTTL = 5 * time.Minute

const ModelPlazaDescriptionMaxLength = 4000

type ModelPlazaOfficialPricing struct {
	InputPrice        *float64
	OutputPrice       *float64
	CacheWritePrice   *float64
	CacheWrite1hPrice *float64
	CacheReadPrice    *float64
}

type ModelPlazaModel struct {
	Name            string
	Platform        string
	Pricing         *ChannelModelPricing
	OfficialPricing *ModelPlazaOfficialPricing
}

type ModelPlazaGroup struct {
	ID                 int64
	Name               string
	Description        string
	Platform           string
	SubscriptionType   string
	RateMultiplier     float64
	PeakRateEnabled    bool
	PeakStart          string
	PeakEnd            string
	PeakRateMultiplier float64
	IsExclusive        bool
	Models             []ModelPlazaModel
}

type modelPlazaSnapshot struct {
	groups   []ModelPlazaGroup
	builtAt  time.Time
	revision uint64
}

// ModelPlazaService owns a cold-path snapshot independent from ChannelService's
// gateway cache. It is only rebuilt by plaza reads after TTL expiry or a CRUD
// invalidation mark.
type ModelPlazaService struct {
	channelRepo ChannelRepository
	groupRepo   GroupRepository
	pricing     *PricingService
	snapshot    atomic.Pointer[modelPlazaSnapshot]
	dirty       atomic.Bool
	revision    atomic.Uint64
	rebuild     singleflight.Group
}

func NewModelPlazaService(channelRepo ChannelRepository, groupRepo GroupRepository, pricing *PricingService) *ModelPlazaService {
	s := &ModelPlazaService{channelRepo: channelRepo, groupRepo: groupRepo, pricing: pricing}
	s.dirty.Store(true)
	return s
}

func (s *ModelPlazaService) Invalidate() {
	if s != nil {
		s.revision.Add(1)
		s.dirty.Store(true)
	}
}

func (s *ModelPlazaService) List(ctx context.Context) ([]ModelPlazaGroup, error) {
	if s == nil || s.channelRepo == nil || s.groupRepo == nil {
		return nil, fmt.Errorf("model plaza is unavailable")
	}
	if current := s.snapshot.Load(); current != nil && !s.dirty.Load() && time.Since(current.builtAt) < modelPlazaSnapshotTTL {
		return cloneModelPlazaGroups(current.groups), nil
	}
	value, err, _ := s.rebuild.Do("snapshot", func() (any, error) {
		if current := s.snapshot.Load(); current != nil && !s.dirty.Load() && time.Since(current.builtAt) < modelPlazaSnapshotTTL {
			return current, nil
		}
		buildRevision := s.revision.Load()
		groups, buildErr := s.build(ctx)
		if buildErr != nil {
			return nil, buildErr
		}
		next := &modelPlazaSnapshot{groups: groups, builtAt: time.Now(), revision: buildRevision}
		s.snapshot.Store(next)
		s.dirty.Store(false)
		// CRUD invalidation racing with this cold-path rebuild must not be lost.
		if s.revision.Load() != buildRevision {
			s.dirty.Store(true)
		}
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	return cloneModelPlazaGroups(value.(*modelPlazaSnapshot).groups), nil
}

func (s *ModelPlazaService) build(ctx context.Context) ([]ModelPlazaGroup, error) {
	channels, err := s.channelRepo.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list model plaza channels: %w", err)
	}
	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("list model plaza groups: %w", err)
	}
	sort.SliceStable(channels, func(i, j int) bool { return strings.ToLower(channels[i].Name) < strings.ToLower(channels[j].Name) })
	byGroup := make(map[int64]*ModelPlazaGroup, len(groups))
	order := make([]int64, 0, len(groups))
	for i := range groups {
		g := &groups[i]
		byGroup[g.ID] = &ModelPlazaGroup{
			ID: g.ID, Name: g.Name, Description: g.Description, Platform: g.Platform,
			SubscriptionType: g.SubscriptionType, RateMultiplier: g.RateMultiplier,
			PeakRateEnabled: g.PeakRateEnabled, PeakStart: g.PeakStart, PeakEnd: g.PeakEnd,
			PeakRateMultiplier: g.PeakRateMultiplier, IsExclusive: g.IsExclusive,
		}
		order = append(order, g.ID)
	}
	modelIndexes := make(map[int64]map[string]int, len(groups))
	for i := range channels {
		channel := &channels[i]
		if channel.Status != StatusActive {
			continue
		}
		models := channel.SupportedModels()
		fillModelPlazaPricingFallback(models, s.pricing)
		for _, groupID := range channel.GroupIDs {
			group, ok := byGroup[groupID]
			if !ok {
				continue
			}
			index := modelIndexes[groupID]
			if index == nil {
				index = make(map[string]int, len(models))
				modelIndexes[groupID] = index
			}
			for j := range models {
				model := &models[j]
				if model.Platform != group.Platform {
					continue
				}
				key := strings.ToLower(model.Name)
				if at, found := index[key]; found {
					if group.Models[at].Pricing == nil && model.Pricing != nil {
						group.Models[at].Pricing = cloneChannelModelPricing(model.Pricing)
					}
					continue
				}
				index[key] = len(group.Models)
				group.Models = append(group.Models, ModelPlazaModel{Name: model.Name, Platform: model.Platform, Pricing: cloneChannelModelPricing(model.Pricing)})
			}
		}
	}
	official := make(map[string]*ModelPlazaOfficialPricing)
	result := make([]ModelPlazaGroup, 0, len(order))
	for _, groupID := range order {
		group := byGroup[groupID]
		if len(group.Models) == 0 {
			continue
		}
		sort.SliceStable(group.Models, func(i, j int) bool { return group.Models[i].Name < group.Models[j].Name })
		for i := range group.Models {
			group.Models[i].OfficialPricing = s.officialPricing(group.Models[i].Name, official)
		}
		result = append(result, *group)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].RateMultiplier != result[j].RateMultiplier {
			return result[i].RateMultiplier < result[j].RateMultiplier
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func fillModelPlazaPricingFallback(models []SupportedModel, pricing *PricingService) {
	if pricing == nil {
		return
	}
	for i := range models {
		if !pricingNeedsFallback(models[i].Pricing) {
			continue
		}
		if global := pricing.GetModelPricing(models[i].Name); global != nil {
			models[i].Pricing = synthesizePricingFromLiteLLM(global, models[i].Pricing)
		}
	}
}

func (s *ModelPlazaService) officialPricing(model string, memo map[string]*ModelPlazaOfficialPricing) *ModelPlazaOfficialPricing {
	if cached, ok := memo[model]; ok {
		return cached
	}
	if s.pricing == nil {
		memo[model] = nil
		return nil
	}
	var result *ModelPlazaOfficialPricing
	if p := s.pricing.GetModelPricing(model); p != nil && !p.TokenPricingAbsent {
		result = &ModelPlazaOfficialPricing{
			InputPrice: nonZeroPtr(p.InputCostPerToken), OutputPrice: nonZeroPtr(p.OutputCostPerToken),
			CacheWritePrice:   nonZeroPtr(p.CacheCreationInputTokenCost),
			CacheWrite1hPrice: nonZeroPtr(p.CacheCreationInputTokenCostAbove1hr),
			CacheReadPrice:    nonZeroPtr(p.CacheReadInputTokenCost),
		}
		if result.InputPrice == nil && result.OutputPrice == nil && result.CacheWritePrice == nil && result.CacheWrite1hPrice == nil && result.CacheReadPrice == nil {
			result = nil
		}
	}
	memo[model] = result
	return result
}

func cloneChannelModelPricing(source *ChannelModelPricing) *ChannelModelPricing {
	if source == nil {
		return nil
	}
	clone := source.Clone()
	clone.InputPrice = cloneFloat64Pointer(source.InputPrice)
	clone.OutputPrice = cloneFloat64Pointer(source.OutputPrice)
	clone.CacheWritePrice = cloneFloat64Pointer(source.CacheWritePrice)
	clone.CacheReadPrice = cloneFloat64Pointer(source.CacheReadPrice)
	clone.ImageInputPrice = cloneFloat64Pointer(source.ImageInputPrice)
	clone.ImageOutputPrice = cloneFloat64Pointer(source.ImageOutputPrice)
	clone.PerRequestPrice = cloneFloat64Pointer(source.PerRequestPrice)
	for i := range clone.Intervals {
		clone.Intervals[i].MaxTokens = cloneIntPointer(source.Intervals[i].MaxTokens)
		clone.Intervals[i].InputPrice = cloneFloat64Pointer(source.Intervals[i].InputPrice)
		clone.Intervals[i].OutputPrice = cloneFloat64Pointer(source.Intervals[i].OutputPrice)
		clone.Intervals[i].CacheWritePrice = cloneFloat64Pointer(source.Intervals[i].CacheWritePrice)
		clone.Intervals[i].CacheReadPrice = cloneFloat64Pointer(source.Intervals[i].CacheReadPrice)
		clone.Intervals[i].PerRequestPrice = cloneFloat64Pointer(source.Intervals[i].PerRequestPrice)
	}
	return &clone
}

func cloneFloat64Pointer(source *float64) *float64 {
	if source == nil {
		return nil
	}
	value := *source
	return &value
}

func cloneIntPointer(source *int) *int {
	if source == nil {
		return nil
	}
	value := *source
	return &value
}

func cloneModelPlazaOfficialPricing(source *ModelPlazaOfficialPricing) *ModelPlazaOfficialPricing {
	if source == nil {
		return nil
	}
	return &ModelPlazaOfficialPricing{
		InputPrice:        cloneFloat64Pointer(source.InputPrice),
		OutputPrice:       cloneFloat64Pointer(source.OutputPrice),
		CacheWritePrice:   cloneFloat64Pointer(source.CacheWritePrice),
		CacheWrite1hPrice: cloneFloat64Pointer(source.CacheWrite1hPrice),
		CacheReadPrice:    cloneFloat64Pointer(source.CacheReadPrice),
	}
}

func cloneModelPlazaGroups(source []ModelPlazaGroup) []ModelPlazaGroup {
	result := make([]ModelPlazaGroup, len(source))
	for i := range source {
		result[i] = source[i]
		result[i].Models = make([]ModelPlazaModel, len(source[i].Models))
		for j := range source[i].Models {
			result[i].Models[j] = source[i].Models[j]
			result[i].Models[j].Pricing = cloneChannelModelPricing(source[i].Models[j].Pricing)
			result[i].Models[j].OfficialPricing = cloneModelPlazaOfficialPricing(source[i].Models[j].OfficialPricing)
		}
	}
	return result
}

type ModelPlazaRuntime struct {
	Enabled     bool
	RequireAuth bool
	Description string
}

func (s *SettingService) GetModelPlazaRuntime(ctx context.Context) ModelPlazaRuntime {
	if s == nil || s.settingRepo == nil {
		return ModelPlazaRuntime{RequireAuth: true}
	}
	values, err := s.settingRepo.GetMultiple(ctx, []string{SettingKeyModelPlazaEnabled, SettingKeyModelPlazaRequireAuth, SettingKeyModelPlazaDescription})
	if err != nil {
		return ModelPlazaRuntime{RequireAuth: true}
	}
	return ModelPlazaRuntime{
		Enabled: values[SettingKeyModelPlazaEnabled] == "true", RequireAuth: values[SettingKeyModelPlazaRequireAuth] != "false",
		Description: strings.TrimSpace(values[SettingKeyModelPlazaDescription]),
	}
}
