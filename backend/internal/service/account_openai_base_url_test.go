package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountGetOpenAIBaseURLUsesPlatformDefaults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		platform string
		want     string
	}{
		{name: "openai", platform: PlatformOpenAI, want: "https://api.openai.com"},
		{name: "kimi", platform: PlatformKimi, want: "https://api.moonshot.cn/v1"},
		{name: "zhipu", platform: PlatformZhipu, want: "https://open.bigmodel.cn/api/paas/v4"},
		{name: "deepseek", platform: PlatformDeepSeek, want: "https://api.deepseek.com/v1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			account := &Account{Platform: tt.platform, Type: AccountTypeAPIKey}
			require.Equal(t, tt.want, account.GetOpenAIBaseURL())
		})
	}
}

func TestAccountGetOpenAIBaseURLPreservesCustomBaseURL(t *testing.T) {
	t.Parallel()

	account := &Account{
		Platform: PlatformKimi,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": "https://gateway.example.test/kimi/v1",
		},
	}
	require.Equal(t, "https://gateway.example.test/kimi/v1", account.GetOpenAIBaseURL())
}

func TestAccountGetOpenAIBaseURLRejectsNonCompatiblePlatform(t *testing.T) {
	t.Parallel()

	require.Empty(t, (&Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}).GetOpenAIBaseURL())
}
