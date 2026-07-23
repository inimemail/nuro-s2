package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	maxGroupNameRunes            = 100
	duplicateGroupInactiveStatus = "inactive"
)

type AdminGroupDuplicateRepository interface {
	FindByDuplicateOperationID(context.Context, string) (*Group, error)
	CreateFromSource(context.Context, *Group, int64) error
}

type AdminGroupDuplicateService interface {
	DuplicateGroup(context.Context, int64, string, string) (*Group, error)
	RecoverDuplicateGroup(context.Context, int64, string, string) (*Group, error)
}

func adminGroupDuplicateRepositoryFrom(repo GroupRepository) AdminGroupDuplicateRepository {
	duplicateRepo, _ := repo.(AdminGroupDuplicateRepository)
	return duplicateRepo
}

func duplicateGroupOperationID(sourceID int64, actorScope, operationKey string) string {
	operationKey = strings.TrimSpace(operationKey)
	if operationKey == "" {
		return ""
	}
	actorScope = strings.TrimSpace(actorScope)
	if actorScope == "" {
		actorScope = "admin:0"
	}
	digest := sha256.Sum256([]byte("admin.groups.duplicate\x00" + actorScope + "\x00" + strconv.FormatInt(sourceID, 10) + "\x00" + operationKey))
	return fmt.Sprintf("%x", digest)
}

func duplicateGroupName(sourceName string, copyNumber int) string {
	if copyNumber < 1 {
		copyNumber = 1
	}
	suffix := " (Copy)"
	if copyNumber > 1 {
		suffix = fmt.Sprintf(" (Copy %d)", copyNumber)
	}
	base := []rune(strings.TrimSpace(sourceName))
	maxBase := maxGroupNameRunes - len([]rune(suffix))
	if maxBase < 0 {
		maxBase = 0
	}
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	return string(base) + suffix
}

func cloneGroupPointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneGroupModelRouting(value map[string][]int64) map[string][]int64 {
	if value == nil {
		return nil
	}
	clone := make(map[string][]int64, len(value))
	for model, accountIDs := range value {
		clone[model] = append([]int64(nil), accountIDs...)
	}
	return clone
}

func cloneGroupMessagesConfig(value OpenAIMessagesDispatchModelConfig) OpenAIMessagesDispatchModelConfig {
	clone := value
	if value.ExactModelMappings != nil {
		clone.ExactModelMappings = make(map[string]string, len(value.ExactModelMappings))
		for requested, mapped := range value.ExactModelMappings {
			clone.ExactModelMappings[requested] = mapped
		}
	}
	return clone
}

func cloneGroupForDuplicate(source *Group, operationID string) *Group {
	return &Group{
		Name:                               duplicateGroupName(source.Name, 1),
		Description:                        source.Description,
		Platform:                           source.Platform,
		RateMultiplier:                     source.RateMultiplier,
		UpstreamBillingGuardMaxMultiplier:  cloneGroupPointer(source.UpstreamBillingGuardMaxMultiplier),
		PeakRateEnabled:                    source.PeakRateEnabled,
		PeakStart:                          source.PeakStart,
		PeakEnd:                            source.PeakEnd,
		PeakRateMultiplier:                 source.PeakRateMultiplier,
		IsExclusive:                        source.IsExclusive,
		Status:                             duplicateGroupInactiveStatus,
		DuplicateOperationID:               operationID,
		SubscriptionType:                   source.SubscriptionType,
		DailyLimitUSD:                      cloneGroupPointer(source.DailyLimitUSD),
		WeeklyLimitUSD:                     cloneGroupPointer(source.WeeklyLimitUSD),
		MonthlyLimitUSD:                    cloneGroupPointer(source.MonthlyLimitUSD),
		DefaultValidityDays:                source.DefaultValidityDays,
		AllowImageGeneration:               source.AllowImageGeneration,
		AllowBatchImageGeneration:          source.AllowBatchImageGeneration,
		ImageRateIndependent:               source.ImageRateIndependent,
		ImageRateMultiplier:                source.ImageRateMultiplier,
		ImagePrice1K:                       cloneGroupPointer(source.ImagePrice1K),
		ImagePrice2K:                       cloneGroupPointer(source.ImagePrice2K),
		ImagePrice4K:                       cloneGroupPointer(source.ImagePrice4K),
		BatchImageDiscountMultiplier:       source.BatchImageDiscountMultiplier,
		BatchImageHoldMultiplier:           source.BatchImageHoldMultiplier,
		VideoRateIndependent:               source.VideoRateIndependent,
		VideoRateMultiplier:                source.VideoRateMultiplier,
		VideoPrice480P:                     cloneGroupPointer(source.VideoPrice480P),
		VideoPrice720P:                     cloneGroupPointer(source.VideoPrice720P),
		VideoPrice1080P:                    cloneGroupPointer(source.VideoPrice1080P),
		WebSearchPricePerCall:              cloneGroupPointer(source.WebSearchPricePerCall),
		ClaudeCodeOnly:                     source.ClaudeCodeOnly,
		FallbackGroupID:                    cloneGroupPointer(source.FallbackGroupID),
		FallbackGroupIDOnInvalidRequest:    cloneGroupPointer(source.FallbackGroupIDOnInvalidRequest),
		ModelRouting:                       cloneGroupModelRouting(source.ModelRouting),
		ModelRoutingEnabled:                source.ModelRoutingEnabled,
		MCPXMLInject:                       source.MCPXMLInject,
		SupportedModelScopes:               append([]string(nil), source.SupportedModelScopes...),
		SortOrder:                          source.SortOrder,
		AllowMessagesDispatch:              source.AllowMessagesDispatch,
		RequireOAuthOnly:                   source.RequireOAuthOnly,
		RequirePrivacySet:                  source.RequirePrivacySet,
		DefaultMappedModel:                 source.DefaultMappedModel,
		MessagesDispatchModelConfig:        cloneGroupMessagesConfig(source.MessagesDispatchModelConfig),
		ModelsListConfig:                   GroupModelsListConfig{Enabled: source.ModelsListConfig.Enabled, Models: append([]string(nil), source.ModelsListConfig.Models...)},
		StrictModelPriorityOnModelMismatch: source.StrictModelPriorityOnModelMismatch,
		RPMLimit:                           source.RPMLimit,
		MaxReasoningEffort:                 source.MaxReasoningEffort,
		ReasoningEffortMappings:            append([]ReasoningEffortMapping(nil), source.ReasoningEffortMappings...),
	}
}

func (s *adminServiceImpl) RecoverDuplicateGroup(ctx context.Context, id int64, actorScope, operationKey string) (*Group, error) {
	operationID := duplicateGroupOperationID(id, actorScope, operationKey)
	if operationID == "" {
		return nil, nil
	}
	if s.groupDuplicateRepo == nil {
		return nil, errors.New("group duplicate repository is not configured")
	}
	group, err := s.groupDuplicateRepo.FindByDuplicateOperationID(ctx, operationID)
	if err != nil || group == nil {
		return group, err
	}
	return s.groupRepo.GetByID(ctx, group.ID)
}

func (s *adminServiceImpl) DuplicateGroup(ctx context.Context, id int64, actorScope, operationKey string) (*Group, error) {
	if recovered, err := s.RecoverDuplicateGroup(ctx, id, actorScope, operationKey); err != nil || recovered != nil {
		return recovered, err
	}
	source, err := s.groupRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.groupDuplicateRepo == nil {
		return nil, errors.New("group duplicate repository is not configured")
	}
	duplicate := cloneGroupForDuplicate(source, duplicateGroupOperationID(id, actorScope, operationKey))
	sanitizeGroupReasoningEffortPolicy(duplicate)
	for copyNumber := 1; ; copyNumber++ {
		duplicate.Name = duplicateGroupName(source.Name, copyNumber)
		duplicate.ID = 0
		duplicate.CreatedAt = time.Time{}
		duplicate.UpdatedAt = time.Time{}
		if err := s.groupDuplicateRepo.CreateFromSource(ctx, duplicate, source.ID); err == nil {
			return s.groupRepo.GetByID(ctx, duplicate.ID)
		} else if !errors.Is(err, ErrGroupExists) {
			return nil, fmt.Errorf("create duplicate group: %w", err)
		}
		if recovered, err := s.RecoverDuplicateGroup(ctx, id, actorScope, operationKey); err != nil || recovered != nil {
			return recovered, err
		}
	}
}
