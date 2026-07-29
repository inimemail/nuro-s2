//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func newModelPlazaTestService(channels []Channel, groups []Group, pricing *PricingService) *ModelPlazaService {
	channelRepo := &mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) {
		return channels, nil
	}}
	return NewModelPlazaService(channelRepo, &stubGroupRepoForAvailable{activeGroups: groups}, pricing)
}

func modelPlazaTestChannel(id int64, name string, groupIDs []int64, platform string, models ...string) Channel {
	return Channel{
		ID: id, Name: name, Status: StatusActive, GroupIDs: groupIDs,
		ModelPricing: []ChannelModelPricing{{
			Platform: platform, Models: models, BillingMode: BillingModeToken,
			InputPrice: testPtrFloat64(3e-6), OutputPrice: testPtrFloat64(15e-6),
		}},
	}
}

func TestModelPlazaBuildAggregatesAndIsolatesPlatforms(t *testing.T) {
	channels := []Channel{
		modelPlazaTestChannel(1, "anthropic-a", []int64{10}, PlatformAnthropic, "claude-sonnet"),
		modelPlazaTestChannel(2, "anthropic-b", []int64{10}, PlatformAnthropic, "claude-opus"),
		modelPlazaTestChannel(3, "openai", []int64{10, 20}, PlatformOpenAI, "gpt-5"),
	}
	groups := []Group{
		{ID: 10, Name: "anthropic", Platform: PlatformAnthropic, RateMultiplier: 1},
		{ID: 20, Name: "openai", Platform: PlatformOpenAI, RateMultiplier: 0.5},
		{ID: 30, Name: "empty", Platform: PlatformOpenAI, RateMultiplier: 1},
	}

	got, err := newModelPlazaTestService(channels, groups, nil).List(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "openai", got[0].Name, "groups are sorted by effective base rate")
	require.Equal(t, []string{"gpt-5"}, []string{got[0].Models[0].Name})
	require.Equal(t, "anthropic", got[1].Name)
	require.Equal(t, []string{"claude-opus", "claude-sonnet"}, []string{got[1].Models[0].Name, got[1].Models[1].Name})
}

func TestModelPlazaSkipsInactiveChannelsAndUpgradesDuplicatePricing(t *testing.T) {
	unpriced := Channel{
		ID: 1, Name: "a", Status: StatusActive, GroupIDs: []int64{10},
		ModelMapping: map[string]map[string]string{PlatformAnthropic: {"claude-sonnet": "claude-sonnet"}},
	}
	priced := modelPlazaTestChannel(2, "b", []int64{10}, PlatformAnthropic, "claude-sonnet")
	inactive := modelPlazaTestChannel(3, "c", []int64{10}, PlatformAnthropic, "claude-opus")
	inactive.Status = "inactive"

	got, err := newModelPlazaTestService([]Channel{priced, inactive, unpriced}, []Group{{ID: 10, Name: "g", Platform: PlatformAnthropic}}, nil).List(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Models, 1)
	require.Equal(t, "claude-sonnet", got[0].Models[0].Name)
	require.NotNil(t, got[0].Models[0].Pricing)
}

func TestModelPlazaSnapshotReturnsDeepClonesAndInvalidates(t *testing.T) {
	input := testPtrFloat64(3e-6)
	maxTokens := 200000
	channels := []Channel{{
		ID: 1, Name: "channel", Status: StatusActive, GroupIDs: []int64{10},
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformAnthropic, Models: []string{"claude-sonnet"}, BillingMode: BillingModeToken,
			InputPrice: input, Intervals: []PricingInterval{{MaxTokens: &maxTokens, InputPrice: input}},
		}},
	}}
	groups := []Group{{ID: 10, Name: "group", Platform: PlatformAnthropic}}
	repoCalls := 0
	repo := &mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) {
		repoCalls++
		return channels, nil
	}}
	svc := NewModelPlazaService(repo, &stubGroupRepoForAvailable{activeGroups: groups}, nil)

	first, err := svc.List(context.Background())
	require.NoError(t, err)
	*first[0].Models[0].Pricing.InputPrice = 99
	*first[0].Models[0].Pricing.Intervals[0].MaxTokens = 1

	second, err := svc.List(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, repoCalls)
	require.InDelta(t, 3e-6, *second[0].Models[0].Pricing.InputPrice, 1e-12)
	require.Equal(t, 200000, *second[0].Models[0].Pricing.Intervals[0].MaxTokens)

	svc.Invalidate()
	_, err = svc.List(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, repoCalls)
}

func TestModelPlazaRepositoryErrorsAreWrapped(t *testing.T) {
	sentinel := errors.New("repository unavailable")
	svc := NewModelPlazaService(
		&mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) { return nil, sentinel }},
		&stubGroupRepoForAvailable{}, nil,
	)
	_, err := svc.List(context.Background())
	require.ErrorIs(t, err, sentinel)
}
