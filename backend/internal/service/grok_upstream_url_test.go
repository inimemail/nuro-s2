//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBuildGrokBillingURLUsesOAuthCustomBaseURL(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"base_url": "https://relay.example.com/v1",
		},
	}

	weekly, err := buildGrokBillingURL(account, nil, true)
	require.NoError(t, err)
	require.Equal(t, "https://relay.example.com/v1/billing?format=credits", weekly)

	monthly, err := buildGrokBillingURL(account, nil, false)
	require.NoError(t, err)
	require.Equal(t, "https://relay.example.com/v1/billing", monthly)
}

func TestBuildGrokBillingURLRejectsUnsafeOAuthCustomBaseURLWithoutLeakingIt(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"base_url": "https://private.internal.example/v1?secret=1",
		},
	}

	_, err := buildGrokBillingURL(account, nil, true)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private.internal.example")
	require.NotContains(t, err.Error(), "secret=1")
}

func TestBuildGrokBillingURLAppliesRuntimeUpstreamAllowlist(t *testing.T) {
	account := &Account{
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"base_url": "https://relay.example.com/v1",
		},
	}
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = true
	cfg.Security.URLAllowlist.UpstreamHosts = []string{"allowed.example.com"}

	_, err := buildGrokBillingURL(account, cfg, false)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "relay.example.com")

	cfg.Security.URLAllowlist.UpstreamHosts = []string{"relay.example.com"}
	url, err := buildGrokBillingURL(account, cfg, false)
	require.NoError(t, err)
	require.Equal(t, "https://relay.example.com/v1/billing", url)
}
