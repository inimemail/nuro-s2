package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type v025UsageRepo struct {
	AccountRepository
	account *Account
	writes  atomic.Int32
}

func (r *v025UsageRepo) GetByID(context.Context, int64) (*Account, error) { return r.account, nil }
func (r *v025UsageRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	r.writes.Add(1)
	return nil
}

type v025UsageUpstream struct {
	HTTPUpstream
	started chan struct{}
	finish  chan struct{}
}

func (u *v025UsageUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	select {
	case u.started <- struct{}{}:
	default:
	}
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-u.finish:
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`<section>Session usage 100% Resets in 1 hour</section>`))}, nil
	}
}

func TestOllamaRecoveryNoPrematureWriteAndShutdown(t *testing.T) {
	for _, cancelProbe := range []bool{false, true} {
		repo := &v025UsageRepo{account: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Credentials: map[string]any{"base_url": "https://ollama.com/v1"},
			Extra:       map[string]any{OllamaCloudUsageSessionExtraKey: "OLD:session=test", OllamaCloudUsageRateLimitRecoveryExtraKey: true}}}
		upstream := &v025UsageUpstream{started: make(chan struct{}, 1), finish: make(chan struct{})}
		svc := NewOllamaCloudUsageService(repo, upstream, nil, channelMonitorRoundtripEncryptor{}, true)
		t.Cleanup(svc.Stop)
		callback := make(chan bool, 1)
		require.True(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(1, func(ctx context.Context, reset time.Time) {
			callback <- repo.writes.Load() == 0 && ctx.Err() == nil && reset.After(time.Now())
		}))
		select {
		case <-upstream.started:
		case <-time.After(time.Second):
			t.Fatal("probe not started")
		}
		require.False(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(1, func(context.Context, time.Time) {}))
		if cancelProbe {
			svc.Stop()
			select {
			case <-callback:
				t.Fatal("cancelled probe invoked callback")
			default:
			}
		} else {
			close(upstream.finish)
			select {
			case valid := <-callback:
				require.True(t, valid)
			case <-time.After(time.Second):
				t.Fatal("missing callback")
			}
			svc.Stop()
		}
		require.Zero(t, repo.writes.Load())
		require.False(t, svc.ScheduleOllamaCloudUsageRateLimitProbe(1, func(context.Context, time.Time) {}))
	}
}

func TestOpenCodeQuotaWindowsAndZenEligibility(t *testing.T) {
	tiers := parseOpenCodeGoUsageTiers([]byte(`{"usage":{"rolling":{"percent":12,"resetsAt":"2026-09-20T12:00:00Z"},"weekly":{"percent":75},"monthly":{"percent":90}}}`))
	require.Len(t, tiers, 3)
	require.Equal(t, "5h", tiers[0].Window)
	require.Equal(t, "monthly", tiers[2].Window)
	require.Equal(t, "2026-09-20T12:00:00Z", tiers[0].ResetAt)
	require.Empty(t, parseOpenCodeGoUsageTiers([]byte(`{"usage":{"rolling":{"percent":-1}}}`)))
	a := &Account{Platform: PlatformOpenCodeGo, Type: AccountTypeAPIKey, Credentials: map[string]any{"account_mode": "go"}}
	require.NoError(t, validateCodingPlanAccount(a))
	require.NoError(t, monitorAccountQuotaCapability(a))
	a.Credentials["account_mode"] = "zen"
	require.Error(t, validateCodingPlanAccount(a))
	require.Error(t, monitorAccountQuotaCapability(a))
}

func TestClaudeOAuthCacheControlPreservation(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"You are OpenCode, the best coding agent on the planet.","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	got, _ := normalizeClaudeOAuthRequestBody(body, "claude-sonnet-4-6", claudeOAuthNormalizeOptions{})
	require.Equal(t, "1h", gjson.GetBytes(got, "system.0.cache_control.ttl").String())
	require.NotContains(t, gjson.GetBytes(got, "system.0.text").String(), "You are OpenCode")
}
