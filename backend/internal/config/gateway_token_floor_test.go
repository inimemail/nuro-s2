package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestGatewayMinOutputTokensValidation(t *testing.T) {
	cfg := buildValid(t)
	require.Equal(t, 128, cfg.Gateway.MinOutputTokens)
	for _, limit := range []int{0, 16, 512} {
		cfg.Gateway.MinOutputTokens = limit
		require.NoError(t, cfg.Validate())
		require.GreaterOrEqual(t, cfg.Gateway.MinOutputTokens, 16)
	}
	for _, limit := range []int{-1, 1, 15} {
		cfg.Gateway.MinOutputTokens = limit
		require.ErrorContains(t, cfg.Validate(), "gateway.min_output_tokens")
	}
}
