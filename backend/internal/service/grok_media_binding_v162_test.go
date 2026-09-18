package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGrokMediaVideoBindingV162IncludesUserAndAPIKey(t *testing.T) {
	base := GrokMediaVideoRequestSessionHash("request-1", 10, 20)
	require.NotEmpty(t, base)
	require.NotEqual(t, base, GrokMediaVideoRequestSessionHash("request-1", 11, 20))
	require.NotEqual(t, base, GrokMediaVideoRequestSessionHash("request-1", 10, 21))
	require.NotEqual(t, base, GrokMediaVideoRequestSessionHash("request-2", 10, 20))
	require.Empty(t, GrokMediaVideoRequestSessionHash("", 10, 20))
}

func TestGrokMediaVideoBindingV162MissDoesNotFallBackToScheduler(t *testing.T) {
	selection, exactRoute, err := (&OpenAIGatewayService{}).
		SelectBoundGrokMediaVideoRequestAccount(context.Background(), nil, "request-1", 10, 20)
	require.Nil(t, selection)
	require.True(t, exactRoute)
	require.ErrorIs(t, err, ErrGrokMediaVideoBindingUnavailable)
}

func TestGrokMediaVideoBindingV162FailsClosedWithoutCache(t *testing.T) {
	err := (&OpenAIGatewayService{}).
		BindGrokMediaVideoRequestAccount(context.Background(), nil, "request-1", 10, 20, 30)
	require.ErrorIs(t, err, ErrGrokMediaVideoBindingUnavailable)
}

func TestGrokMediaVideoBindingTTLDoesNotShortenAsyncTaskLifetime(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.StickySessionTTLSeconds = 60
	svc := &OpenAIGatewayService{cfg: cfg}

	require.Equal(t, 24*time.Hour, svc.grokMediaVideoBindingTTL())
}
