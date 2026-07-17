package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGrokMediaRequiredCapabilityV160(t *testing.T) {
	for _, endpoint := range []service.GrokMediaEndpoint{
		service.GrokMediaEndpointImagesGenerations,
		service.GrokMediaEndpointImagesEdits,
		service.GrokMediaEndpointVideosGenerations,
		service.GrokMediaEndpointVideosEdits,
		service.GrokMediaEndpointVideosExtensions,
	} {
		require.Equal(t, service.OpenAIEndpointCapabilityGrokMediaGeneration, grokMediaRequiredCapability(endpoint))
	}
	require.Empty(t, grokMediaRequiredCapability(service.GrokMediaEndpointVideoStatus))
}

func TestAcquireImageGenerationSlotForWSV160ReleasesPerTurn(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.ImageConcurrency.Enabled = true
	cfg.Gateway.ImageConcurrency.MaxConcurrentRequests = 1
	cfg.Gateway.ImageConcurrency.OverflowMode = config.ImageConcurrencyOverflowModeReject
	h := &OpenAIGatewayHandler{cfg: cfg, imageLimiter: &imageConcurrencyLimiter{}}

	release, acquired := h.acquireImageGenerationSlotForWS(context.Background())
	require.True(t, acquired)
	require.NotNil(t, release)
	_, acquired = h.acquireImageGenerationSlotForWS(context.Background())
	require.False(t, acquired)
	release()

	release, acquired = h.acquireImageGenerationSlotForWS(context.Background())
	require.True(t, acquired)
	require.NotNil(t, release)
	release()
}
