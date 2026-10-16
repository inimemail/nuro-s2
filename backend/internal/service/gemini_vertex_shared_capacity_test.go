//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVertexSharedCapacity429DoesNotCoolDownAccountQuota(t *testing.T) {
	const shared = `{"error":{"code":429,"message":"Resource exhausted. Please try again later.","status":"RESOURCE_EXHAUSTED"}}`
	for _, tc := range []struct {
		name   string
		body   string
		shared bool
	}{
		{"capacity", shared, true},
		{"array envelope", `[` + shared + `]`, true},
		{"quota", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Resource exhausted quota. Please try again later."}}`, false},
		{"reserved throughput", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Resource exhausted provisioned throughput. Please try again later."}}`, false},
		{"retry detail", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Resource exhausted. Please try again later.","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"60s"}]}}`, false},
		{"different error", `{"error":{"status":"UNAVAILABLE","message":"Resource exhausted. Please try again later."}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.shared, isVertexSharedCapacity429([]byte(tc.body))) })
	}
	for _, tc := range []struct {
		name string
		sa   bool
		body string
		want int
	}{
		{"shared capacity SA", true, shared, 0},
		{"SA quota", true, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"Project quota exceeded"}}`, 1},
		{"third party account", false, shared, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &epTrackingRepo{}
			svc := &GeminiMessagesCompatService{accountRepo: repo}
			account := &Account{ID: 42, Platform: PlatformGemini, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://gateway.example.com"}}
			if tc.sa {
				account.Type = AccountTypeServiceAccount
			}
			svc.handleGeminiUpstreamError(context.Background(), account, http.StatusTooManyRequests, nil, []byte(tc.body))
			require.Equal(t, tc.want, repo.rateLimitedCalls)
		})
	}
}
