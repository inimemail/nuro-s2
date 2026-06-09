package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccount_IsAnthropicKiroEnabled(t *testing.T) {
	t.Run("enabled for anthropic apikey", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"anthropic_kiro": true,
			},
		}
		require.True(t, account.IsAnthropicKiroEnabled())
	})

	t.Run("disabled by default", func(t *testing.T) {
		account := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Extra:    map[string]any{},
		}
		require.False(t, account.IsAnthropicKiroEnabled())
	})

	t.Run("disabled for non anthropic apikey account", func(t *testing.T) {
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				"anthropic_kiro": true,
			},
		}
		require.False(t, account.IsAnthropicKiroEnabled())
	})
}
