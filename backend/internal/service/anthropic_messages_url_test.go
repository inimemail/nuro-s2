package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCloudflareRESTMessagesExactException(t *testing.T) {
	for _, base := range []string{"https://api.cloudflare.com/client/v4/accounts/abc/ai", "https://api.cloudflare.com:443/client/v4/accounts/abc/ai"} {
		require.Equal(t, base+"/v1/messages", anthropicMessagesURL(base))
	}
	for _, raw := range []string{
		"http://api.cloudflare.com/client/v4/accounts/a/ai/v1/messages",
		"https://api.cloudflare.com.evil.test/client/v4/accounts/a/ai/v1/messages",
		"https://u@api.cloudflare.com/client/v4/accounts/a/ai/v1/messages",
		"https://api.cloudflare.com:444/client/v4/accounts/a/ai/v1/messages",
		"https://api.cloudflare.com/client/v4/accounts/a/ai/v1/messages?x=y",
		"https://api.cloudflare.com/client/v4/accounts/a/ai/v1/messages?",
		"https://api.cloudflare.com/client/v4/accounts/a/ai/v1/messages#x",
		"https://api.cloudflare.com/client/v4/accounts//ai/v1/messages",
		"https://api.cloudflare.com/client/v4/accounts/a/ai/v1/%6dessages",
		"https://gateway.ai.cloudflare.com/v1/a/b/anthropic/v1/messages",
	} {
		require.False(t, isCloudflareRESTMessagesURL(raw), raw)
	}
	require.Equal(t, "https://api.anthropic.com/v1/messages?beta=true", anthropicMessagesURL("https://api.anthropic.com"))
}
