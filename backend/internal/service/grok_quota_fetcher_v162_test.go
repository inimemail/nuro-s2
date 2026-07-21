package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

func TestGrokQuotaFetcherV162ExposesFreeTokenLimit(t *testing.T) {
	usage := NewGrokQuotaFetcher().BuildUsageInfo(nil)
	require.Equal(t, xai.GrokFreeRolling24hTokenLimit, usage.GrokFreeTokenLimit)
}
