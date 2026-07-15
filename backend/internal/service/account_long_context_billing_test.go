//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountIsOpenAILongContextBillingEnabled(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    bool
	}{
		{name: "nil account", account: nil},
		{name: "non OpenAI account", account: &Account{Platform: PlatformGrok}},
		{name: "missing value", account: &Account{Platform: PlatformOpenAI}},
		{name: "malformed value", account: &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAILongContextBillingEnabledKey: "true"}}},
		{name: "explicit false", account: &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAILongContextBillingEnabledKey: false}}},
		{name: "explicit true", account: &Account{Platform: PlatformOpenAI, Extra: map[string]any{openAILongContextBillingEnabledKey: true}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.account.IsOpenAILongContextBillingEnabled())
		})
	}
}

func TestNormalizeOpenAILongContextBillingExtra(t *testing.T) {
	extra, err := normalizeOpenAILongContextBillingExtra(PlatformOpenAI, nil)
	require.NoError(t, err)
	require.Equal(t, false, extra[openAILongContextBillingEnabledKey])

	_, err = normalizeOpenAILongContextBillingExtra(PlatformOpenAI, map[string]any{
		openAILongContextBillingEnabledKey: "false",
	})
	require.Error(t, err)

	nonOpenAIExtra := map[string]any{openAILongContextBillingEnabledKey: "provider-owned"}
	normalized, err := normalizeOpenAILongContextBillingExtra(PlatformGrok, nonOpenAIExtra)
	require.NoError(t, err)
	require.Equal(t, nonOpenAIExtra, normalized)
}
