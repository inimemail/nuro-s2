package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAICompatibleRequestPlatform_PreservesCompositeTarget(t *testing.T) {
	apiKey := &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite}}
	ctx := service.WithResolvedTargetPlatform(context.Background(), service.PlatformGrok)

	require.Equal(t, service.PlatformGrok, openAICompatibleRequestPlatform(ctx, apiKey))
	require.Equal(t, service.PlatformComposite, openAICompatibleRequestPlatform(context.Background(), apiKey))
}

func TestOpenAICompatibleRequestPlatform_PreservesDomesticProvider(t *testing.T) {
	for _, platform := range []string{service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepSeek} {
		apiKey := &service.APIKey{Group: &service.Group{Platform: platform}}
		require.Equal(t, platform, openAICompatibleRequestPlatform(context.Background(), apiKey))
	}
}
