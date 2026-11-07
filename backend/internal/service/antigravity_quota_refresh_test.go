package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestAntigravityOptionalRefreshConcurrencyCancellationAndAuth(t *testing.T) {
	for _, scenario := range []string{"success", "partial", "cancel", "auth"} {
		t.Run(scenario, func(t *testing.T) {
			started := make(chan struct{}, 2)
			release := make(chan struct{})
			var active, peak, requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("missing account credential")
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, ":fetchAvailableModels") {
					_, _ = w.Write([]byte(`{"models":{"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.75}}}}`))
					return
				}
				count := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); count > old && !peak.CompareAndSwap(old, count); old = peak.Load() {
				}
				started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				tier := strings.HasSuffix(r.URL.Path, ":loadCodeAssist")
				if scenario == "auth" {
					if tier {
						w.WriteHeader(http.StatusUnauthorized)
					} else {
						w.WriteHeader(http.StatusForbidden)
					}
					return
				}
				if scenario == "partial" && !tier {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if tier {
					_, _ = w.Write([]byte(`{"currentTier":{"id":"pro-tier"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"groups":[{"displayName":"Argon","buckets":[{"bucketId":"argon-five","window":"5h","remainingFraction":0.4}]}]}`))
			}))
			defer server.Close()
			oldURLs, oldAvailability := antigravity.BaseURLs, antigravity.DefaultURLAvailability
			antigravity.BaseURLs = []string{server.URL}
			antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)
			defer func() { antigravity.BaseURLs, antigravity.DefaultURLAvailability = oldURLs, oldAvailability }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type fetched struct {
				result *QuotaResult
				err    error
			}
			done := make(chan fetched, 1)
			go func() {
				result, err := (&AntigravityQuotaFetcher{}).FetchQuota(ctx, &Account{Credentials: map[string]any{"access_token": "fixture-token", "project_id": "fixture-project"}}, "")
				done <- fetched{result, err}
			}()
			for range 2 {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					cancel()
					t.Fatal("optional reads did not run concurrently")
				}
			}
			require.EqualValues(t, 2, peak.Load())
			if scenario == "cancel" {
				cancel()
			}
			close(release)
			var result fetched
			select {
			case result = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("shared cancellation did not finish refresh")
			}
			require.NoError(t, result.err)
			info := result.result.UsageInfo
			require.Contains(t, info.AntigravityQuota, "gemini-2.5-pro")
			require.EqualValues(t, 3, requests.Load())
			switch scenario {
			case "success":
				require.Equal(t, "PRO", info.SubscriptionTier)
				require.Len(t, info.AntigravityQuotaGroups, 1)
				require.NotNil(t, info.AntigravityQuotaSources["summary"].UpdatedAt)
			case "partial":
				require.Equal(t, "PRO", info.SubscriptionTier)
				require.Equal(t, "refresh_failed", info.AntigravityQuotaSources["summary"].Error)
				require.Nil(t, info.AntigravityQuotaSources["summary"].UpdatedAt)
			case "cancel":
				require.Equal(t, "refresh_failed", info.AntigravityQuotaSources["summary"].Error)
			case "auth":
				require.True(t, info.NeedsReauth)
				require.True(t, info.IsForbidden)
				require.Equal(t, errorCodeUnauthenticated, info.ErrorCode)
				require.Same(t, info, mergeAntigravityLastGood(info, &UsageInfo{SubscriptionTier: "ULTRA"}))
				require.Empty(t, info.SubscriptionTier)
			}
		})
	}
}

func TestAntigravityLastGoodRetainsSourceTimeAndOwnsValues(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	used := 40.0
	previous := &UsageInfo{UpdatedAt: &at, SubscriptionTier: "PRO", AntigravityQuotaGroups: []*AntigravityQuotaGroup{{Key: "argon", Windows: []*AntigravityQuotaWindow{{Window: "weekly", Utilization: &used}}}}, AntigravityQuotaSources: map[string]*AntigravityQuotaSource{"summary": {UpdatedAt: &at}, "tier": {UpdatedAt: &at}}}
	before, err := json.Marshal(previous)
	require.NoError(t, err)
	current := mergeAntigravityLastGood(&UsageInfo{AntigravityQuotaSources: map[string]*AntigravityQuotaSource{"summary": {Error: "refresh_failed"}}}, previous)
	require.Equal(t, at, *current.AntigravityQuotaSources["summary"].UpdatedAt)
	require.True(t, current.AntigravityQuotaSources["summary"].Stale)
	require.Equal(t, "PRO", current.SubscriptionTier)
	*current.AntigravityQuotaGroups[0].Windows[0].Utilization = 0
	after, err := json.Marshal(previous)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}
