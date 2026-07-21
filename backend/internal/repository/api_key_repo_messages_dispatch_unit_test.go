package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupEntityToService_PreservesMessagesDispatchModelConfig(t *testing.T) {
	group := &dbent.Group{
		ID:                    1,
		Name:                  "openai-dispatch",
		Platform:              service.PlatformOpenAI,
		Status:                service.StatusActive,
		SubscriptionType:      service.SubscriptionTypeStandard,
		RateMultiplier:        1,
		AllowMessagesDispatch: true,
		DefaultMappedModel:    "gpt-5.4",
		MessagesDispatchModelConfig: service.OpenAIMessagesDispatchModelConfig{
			OpusMappedModel:   "gpt-5.4-nano",
			SonnetMappedModel: "gpt-5.3-codex",
			HaikuMappedModel:  "gpt-5.4-mini",
			ExactModelMappings: map[string]string{
				"claude-sonnet-4.5": "gpt-5.4-nano",
			},
		},
	}

	got := groupEntityToService(group)
	require.NotNil(t, got)
	require.Equal(t, group.MessagesDispatchModelConfig, got.MessagesDispatchModelConfig)
}

func TestAPIKeyRepository_GetByKeyForAuth_PreservesMessagesDispatchModelConfig_SQLite(t *testing.T) {
	repo, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "getbykey-auth-dispatch-unit@test.com")

	guardMultiplier := 2.25
	videoPrice480P := 0.12
	videoPrice720P := 0.24
	videoPrice1080P := 0.36
	webSearchPrice := 0.025
	group, err := client.Group.Create().
		SetName("g-auth-dispatch-unit").
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1).
		SetUpstreamBillingGuardMaxMultiplier(guardMultiplier).
		SetPeakRateEnabled(true).
		SetPeakStart("08:00").
		SetPeakEnd("12:00").
		SetPeakRateMultiplier(1.5).
		SetIsExclusive(true).
		SetAllowBatchImageGeneration(true).
		SetBatchImageDiscountMultiplier(0.4).
		SetBatchImageHoldMultiplier(0.7).
		SetVideoRateIndependent(true).
		SetVideoRateMultiplier(0.8).
		SetVideoPrice480p(videoPrice480P).
		SetVideoPrice720p(videoPrice720P).
		SetVideoPrice1080p(videoPrice1080P).
		SetWebSearchPricePerCall(webSearchPrice).
		SetAllowMessagesDispatch(true).
		SetRequireOauthOnly(true).
		SetRequirePrivacySet(true).
		SetDefaultMappedModel("gpt-5.4").
		SetMessagesDispatchModelConfig(service.OpenAIMessagesDispatchModelConfig{
			OpusMappedModel:   "gpt-5.4-nano",
			SonnetMappedModel: "gpt-5.3-codex",
			HaikuMappedModel:  "gpt-5.4-mini",
			ExactModelMappings: map[string]string{
				"claude-sonnet-4.5": "gpt-5.4-nano",
			},
		}).
		Save(ctx)
	require.NoError(t, err)
	_, err = client.UserAllowedGroup.Create().SetUserID(user.ID).SetGroupID(group.ID).Save(ctx)
	require.NoError(t, err)

	key := &service.APIKey{
		UserID:  user.ID,
		Key:     "sk-getbykey-auth-dispatch-unit",
		Name:    "Dispatch Key Unit",
		GroupID: &group.ID,
		Status:  service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, key))

	got, err := repo.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.Equal(t, key.Name, got.Name)
	require.NotNil(t, got.Group)
	require.Equal(t, group.MessagesDispatchModelConfig, got.Group.MessagesDispatchModelConfig)
	require.Contains(t, got.User.AllowedGroups, group.ID)
	require.True(t, got.Group.IsExclusive)
	require.Equal(t, guardMultiplier, *got.Group.UpstreamBillingGuardMaxMultiplier)
	require.True(t, got.Group.PeakRateEnabled)
	require.Equal(t, 1.5, got.Group.PeakRateMultiplier)
	require.True(t, got.Group.AllowBatchImageGeneration)
	require.Equal(t, 0.4, got.Group.BatchImageDiscountMultiplier)
	require.Equal(t, 0.7, got.Group.BatchImageHoldMultiplier)
	require.True(t, got.Group.VideoRateIndependent)
	require.Equal(t, 0.8, got.Group.VideoRateMultiplier)
	require.Equal(t, videoPrice1080P, *got.Group.VideoPrice1080P)
	require.Equal(t, webSearchPrice, *got.Group.WebSearchPricePerCall)
	require.True(t, got.Group.RequireOAuthOnly)
	require.True(t, got.Group.RequirePrivacySet)
}
