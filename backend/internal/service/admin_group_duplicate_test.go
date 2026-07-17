package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type groupDuplicateRepoStub struct {
	GroupRepository
	source       *Group
	created      map[int64]*Group
	byOperation  map[string]*Group
	createErrors []error
	createCalls  int
	sourceIDs    []int64
}

func (r *groupDuplicateRepoStub) GetByID(_ context.Context, id int64) (*Group, error) {
	if r.source != nil && r.source.ID == id {
		return r.source, nil
	}
	if group := r.created[id]; group != nil {
		return group, nil
	}
	return nil, ErrGroupNotFound
}

func (r *groupDuplicateRepoStub) FindByDuplicateOperationID(_ context.Context, operationID string) (*Group, error) {
	return r.byOperation[operationID], nil
}

func (r *groupDuplicateRepoStub) CreateFromSource(_ context.Context, group *Group, sourceID int64) error {
	r.createCalls++
	r.sourceIDs = append(r.sourceIDs, sourceID)
	if len(r.createErrors) > 0 {
		err := r.createErrors[0]
		r.createErrors = r.createErrors[1:]
		if err != nil {
			return err
		}
	}
	copy := *group
	copy.ID = int64(100 + r.createCalls)
	group.ID = copy.ID
	if r.created == nil {
		r.created = make(map[int64]*Group)
	}
	if r.byOperation == nil {
		r.byOperation = make(map[string]*Group)
	}
	r.created[copy.ID] = &copy
	if copy.DuplicateOperationID != "" {
		r.byOperation[copy.DuplicateOperationID] = &copy
	}
	return nil
}

func TestDuplicateGroupCopiesForkFieldsAndKeepsSourceIndependent(t *testing.T) {
	daily, weekly, monthly := 1.0, 2.0, 3.0
	image1K, image2K, image4K := 0.1, 0.2, 0.4
	video480, video720, video1080 := 0.5, 0.7, 1.0
	webSearch := 0.03
	guardLimit := 1.25
	fallback, invalidFallback := int64(8), int64(9)
	source := &Group{
		ID: 4, Name: "Primary", Description: "description", Platform: PlatformOpenAI,
		RateMultiplier: 1.7, UpstreamBillingGuardMaxMultiplier: &guardLimit,
		PeakRateEnabled: true, PeakStart: "08:00", PeakEnd: "12:00", PeakRateMultiplier: 1.3,
		IsExclusive: true, Status: StatusActive, SubscriptionType: SubscriptionTypeSubscription,
		DailyLimitUSD: &daily, WeeklyLimitUSD: &weekly, MonthlyLimitUSD: &monthly, DefaultValidityDays: 31,
		AllowImageGeneration: true, AllowBatchImageGeneration: true, ImageRateIndependent: true, ImageRateMultiplier: 1.4,
		ImagePrice1K: &image1K, ImagePrice2K: &image2K, ImagePrice4K: &image4K,
		BatchImageDiscountMultiplier: 0.9, BatchImageHoldMultiplier: 1.2,
		VideoRateIndependent: true, VideoRateMultiplier: 1.5,
		VideoPrice480P: &video480, VideoPrice720P: &video720, VideoPrice1080P: &video1080,
		WebSearchPricePerCall: &webSearch, ClaudeCodeOnly: true,
		FallbackGroupID: &fallback, FallbackGroupIDOnInvalidRequest: &invalidFallback,
		ModelRoutingEnabled: true, ModelRouting: map[string][]int64{"gpt-*": {12, 13}}, MCPXMLInject: true,
		SupportedModelScopes: []string{"responses", "messages"}, SortOrder: 7,
		AllowMessagesDispatch: true, RequireOAuthOnly: true, RequirePrivacySet: true,
		DefaultMappedModel: "gpt-5.6", MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
			ExactModelMappings: map[string]string{"gpt-5": "gpt-5.6"},
		},
		ModelsListConfig:                   GroupModelsListConfig{Enabled: true, Models: []string{"gpt-5.6"}},
		StrictModelPriorityOnModelMismatch: true, RPMLimit: 123,
	}
	repo := &groupDuplicateRepoStub{source: source, created: make(map[int64]*Group), byOperation: make(map[string]*Group)}
	svc := &adminServiceImpl{groupRepo: repo, groupDuplicateRepo: repo}

	duplicate, err := svc.DuplicateGroup(context.Background(), source.ID, "admin:5", "request-1")
	require.NoError(t, err)
	require.Equal(t, "Primary (Copy)", duplicate.Name)
	require.Equal(t, duplicateGroupInactiveStatus, duplicate.Status)
	require.Equal(t, []int64{source.ID}, repo.sourceIDs)
	require.Equal(t, source.RateMultiplier, duplicate.RateMultiplier)
	require.Equal(t, source.UpstreamBillingGuardMaxMultiplier, duplicate.UpstreamBillingGuardMaxMultiplier)
	require.NotSame(t, source.UpstreamBillingGuardMaxMultiplier, duplicate.UpstreamBillingGuardMaxMultiplier)
	require.Equal(t, source.PeakRateMultiplier, duplicate.PeakRateMultiplier)
	require.Equal(t, source.ImagePrice4K, duplicate.ImagePrice4K)
	require.Equal(t, source.VideoPrice1080P, duplicate.VideoPrice1080P)
	require.Equal(t, source.WebSearchPricePerCall, duplicate.WebSearchPricePerCall)
	require.Equal(t, source.StrictModelPriorityOnModelMismatch, duplicate.StrictModelPriorityOnModelMismatch)
	require.Equal(t, source.RPMLimit, duplicate.RPMLimit)

	duplicate.ModelRouting["gpt-*"][0] = 99
	*duplicate.UpstreamBillingGuardMaxMultiplier = 2
	duplicate.MessagesDispatchModelConfig.ExactModelMappings["gpt-5"] = "changed"
	duplicate.ModelsListConfig.Models[0] = "changed"
	require.Equal(t, int64(12), source.ModelRouting["gpt-*"][0])
	require.Equal(t, 1.25, *source.UpstreamBillingGuardMaxMultiplier)
	require.Equal(t, "gpt-5.6", source.MessagesDispatchModelConfig.ExactModelMappings["gpt-5"])
	require.Equal(t, "gpt-5.6", source.ModelsListConfig.Models[0])
}

func TestDuplicateGroupRetriesNameCollisionAndRecoversIdempotently(t *testing.T) {
	repo := &groupDuplicateRepoStub{
		source:  &Group{ID: 9, Name: "Group", Status: StatusActive},
		created: make(map[int64]*Group), byOperation: make(map[string]*Group),
		createErrors: []error{ErrGroupExists},
	}
	svc := &adminServiceImpl{groupRepo: repo, groupDuplicateRepo: repo}

	first, err := svc.DuplicateGroup(context.Background(), 9, "admin:2", "same-request")
	require.NoError(t, err)
	require.Equal(t, "Group (Copy 2)", first.Name)
	require.Equal(t, 2, repo.createCalls)

	second, err := svc.DuplicateGroup(context.Background(), 9, "admin:2", "same-request")
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, 2, repo.createCalls)

	_, err = svc.DuplicateGroup(context.Background(), 404, "admin:2", "missing")
	require.True(t, errors.Is(err, ErrGroupNotFound))
}
