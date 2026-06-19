package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type accountUsageOpenAITokenCacheStub struct {
	token string
}

func (s *accountUsageOpenAITokenCacheStub) GetAccessToken(context.Context, string) (string, error) {
	return s.token, nil
}

func (s *accountUsageOpenAITokenCacheStub) SetAccessToken(context.Context, string, string, time.Duration) error {
	return nil
}

func (s *accountUsageOpenAITokenCacheStub) DeleteAccessToken(context.Context, string) error {
	return nil
}

func (s *accountUsageOpenAITokenCacheStub) AcquireRefreshLock(context.Context, string, time.Duration) (bool, error) {
	return true, nil
}

func (s *accountUsageOpenAITokenCacheStub) ReleaseRefreshLock(context.Context, string) error {
	return nil
}

type accountUsageCodexProbeRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh chan map[string]any
	rateLimitCh   chan time.Time
}

func (r *accountUsageCodexProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
	}
	return nil
}

func (r *accountUsageCodexProbeRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

type accountUsageStaticAccountRepo struct {
	stubOpenAIAccountRepo
	account *Account
}

func (r *accountUsageStaticAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func (r *accountUsageStaticAccountRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}

func TestShouldRefreshOpenAICodexSnapshot(t *testing.T) {
	t.Parallel()

	rateLimitedUntil := time.Now().Add(5 * time.Minute)
	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{RateLimitResetAt: &rateLimitedUntil}, usage, now) {
		t.Fatal("expected rate-limited account to force codex snapshot refresh")
	}

	if shouldRefreshOpenAICodexSnapshot(&Account{}, usage, now) {
		t.Fatal("expected complete non-rate-limited usage to skip codex snapshot refresh")
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{}, &UsageInfo{FiveHour: nil, SevenDay: &UsageProgress{}}, now) {
		t.Fatal("expected missing 5h snapshot to require refresh")
	}

	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_enabled": true,
			"codex_usage_updated_at":                       staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale ws snapshot to trigger refresh")
	}
}

func TestExtractOpenAICodexProbeUpdatesAccepts429WithCodexHeaders(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")

	updates, err := extractOpenAICodexProbeUpdates(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
	if err != nil {
		t.Fatalf("extractOpenAICodexProbeUpdates() error = %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected codex probe updates from 429 headers")
	}
	if got := updates["codex_5h_used_percent"]; got != 100.0 {
		t.Fatalf("codex_5h_used_percent = %v, want 100", got)
	}
	if got := updates["codex_7d_used_percent"]; got != 100.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 100", got)
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotOnlyUpdatesExtra(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	svc.persistOpenAICodexProbeSnapshot(321, map[string]any{
		"codex_7d_used_percent": 100.0,
		"codex_7d_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
	})

	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["codex_7d_used_percent"]; got != 100.0 {
			t.Fatalf("codex_7d_used_percent = %v, want 100", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 探测快照写入 extra 超时")
	}

	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将探测快照写入运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_GetOpenAIUsage_DoesNotPromoteCodexExtraToRateLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := &accountUsageCodexProbeRepo{
		rateLimitCh: make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_5h_used_percent": 1.0,
			"codex_5h_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     resetAt.Format(time.RFC3339),
		},
	}

	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 100.0 {
		t.Fatalf("预期 7 天用量仍然可见，实际为 %#v", usage.SevenDay)
	}
	if account.RateLimitResetAt != nil {
		t.Fatalf("不应让已耗尽的 codex extra 改写运行时限流状态: %v", account.RateLimitResetAt)
	}
	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将已耗尽的 codex extra 持久化为运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestExtractOpenAICodexResetCreditUpdates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	updates, supported, ok := extractOpenAICodexResetCreditUpdates([]byte(`{"rate_limit_reset_credits":{"available_count":2}}`), now)
	if !ok {
		t.Fatal("expected reset credit payload to be recognized")
	}
	if !supported {
		t.Fatal("expected reset credits to be supported")
	}
	if got := updates["codex_reset_credits"]; got != 2 {
		t.Fatalf("codex_reset_credits = %v, want 2", got)
	}
	if got := updates["codex_reset_credits_supported"]; got != true {
		t.Fatalf("codex_reset_credits_supported = %v, want true", got)
	}

	updates, supported, ok = extractOpenAICodexResetCreditUpdates([]byte(`{"resetCreditsAvailable":2}`), now)
	if !ok || !supported {
		t.Fatalf("expected camelCase reset credit payload, supported=%v ok=%v", supported, ok)
	}
	if got := updates["codex_reset_credits"]; got != 2 {
		t.Fatalf("codex_reset_credits = %v, want 2 from resetCreditsAvailable", got)
	}

	updates, supported, ok = extractOpenAICodexResetCreditUpdates([]byte(`{"rate_limit_reset_credits":{"available_count":1,"invite_url":"https://chatgpt.com/invite/codex-reset"}}`), now)
	if !ok || !supported {
		t.Fatalf("expected supported reset credit payload with ignored invite URL, supported=%v ok=%v", supported, ok)
	}
	if _, exists := updates["codex_reset_credits_invite_url"]; exists {
		t.Fatalf("did not expect unsupported invite URL field to be persisted: %v", updates)
	}

	updates, supported, ok = extractOpenAICodexResetCreditUpdates([]byte(`{"rate_limit_reset_credits":{}}`), now)
	if !ok {
		t.Fatal("expected missing available_count to be cacheable as zero")
	}
	if !supported {
		t.Fatal("expected reset credits to be supported with zero fallback")
	}
	if got := updates["codex_reset_credits"]; got != 0 {
		t.Fatalf("codex_reset_credits = %v, want 0 when available_count is missing", got)
	}
	if got := updates["codex_reset_credits_supported"]; got != true {
		t.Fatalf("codex_reset_credits_supported = %v, want true", got)
	}
	if got := updates["codex_auto_reset_mode"]; got != openAICodexAutoResetModeOff {
		t.Fatalf("codex_auto_reset_mode = %v, want off", got)
	}

	updates, supported, ok = extractOpenAICodexResetCreditUpdates([]byte(`not json`), now)
	if ok {
		t.Fatal("expected invalid response body to be treated as unknown")
	}
	if supported {
		t.Fatal("expected invalid response support to remain unknown")
	}
	if len(updates) != 0 {
		t.Fatalf("expected no updates for invalid response body, got %v", updates)
	}
}

func TestAccountUsageOpenAIWhamAccessTokenUsesProvider(t *testing.T) {
	t.Parallel()

	account := &Account{
		ID:       1234,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "stale-account-token",
		},
	}
	provider := NewOpenAITokenProvider(nil, &accountUsageOpenAITokenCacheStub{token: "fresh-provider-token"}, nil)
	svc := &AccountUsageService{openAITokenProvider: provider}

	token, err := svc.openAIWhamAccessToken(context.Background(), account)
	if err != nil {
		t.Fatalf("openAIWhamAccessToken() error = %v", err)
	}
	if token != "fresh-provider-token" {
		t.Fatalf("token = %q, want provider token", token)
	}
}

func TestOpenAIWhamChatGPTAccountIDFallsBackToOrganizationID(t *testing.T) {
	t.Parallel()

	got, err := openAIWhamChatGPTAccountID(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"organization_id": "org-fallback",
		},
	})
	if err != nil {
		t.Fatalf("openAIWhamChatGPTAccountID() error = %v", err)
	}
	if got != "org-fallback" {
		t.Fatalf("account id = %q, want organization_id fallback", got)
	}
}

func TestOpenAIWhamChatGPTAccountIDPrefersChatGPTAccountID(t *testing.T) {
	t.Parallel()

	got, err := openAIWhamChatGPTAccountID(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"chatgpt_account_id": "chatgpt-account",
			"organization_id":    "org-fallback",
		},
	})
	if err != nil {
		t.Fatalf("openAIWhamChatGPTAccountID() error = %v", err)
	}
	if got != "chatgpt-account" {
		t.Fatalf("account id = %q, want chatgpt_account_id", got)
	}
}

func TestOpenAIWhamChatGPTAccountIDMissingReturnsStructuredError(t *testing.T) {
	t.Parallel()

	_, err := openAIWhamChatGPTAccountID(&Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected missing account id error")
	}
	if got := infraerrors.Reason(err); got != "OPENAI_QUOTA_MISSING_ACCOUNT_ID" {
		t.Fatalf("reason = %q, want OPENAI_QUOTA_MISSING_ACCOUNT_ID", got)
	}
	if got := infraerrors.Code(err); got != 400 {
		t.Fatalf("code = %d, want 400", got)
	}
}

func TestQueryOpenAIWhamUsagePassesThroughInvites(t *testing.T) {
	var usagePath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usagePath = r.URL.Path
		if got := r.Header.Get("chatgpt-account-id"); got != "chatgpt-acc" {
			t.Fatalf("chatgpt-account-id = %q, want chatgpt-acc", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit_reset_credits":{"available_count":0},"invites":{"created":1,"limit":3,"redeemed":0,"pendingCount":1,"pendingEmails":["friend@example.com"],"slotsLeft":2}}`))
	}))
	defer upstream.Close()

	oldUsageURL := openAIWhamUsageURL
	openAIWhamUsageURL = upstream.URL + "/usage"
	t.Cleanup(func() { openAIWhamUsageURL = oldUsageURL })

	svc := &AccountUsageService{}
	usage, _, err := svc.queryOpenAIWhamUsage(context.Background(), &Account{
		ID:       2026,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "access-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	})
	if err != nil {
		t.Fatalf("queryOpenAIWhamUsage() error = %v", err)
	}
	if usagePath != "/usage" {
		t.Fatalf("usage path = %q, want /usage", usagePath)
	}
	if usage.Invites == nil {
		t.Fatal("expected invites to be passed through")
	}
	if usage.Invites.SlotsLeft != 2 {
		t.Fatalf("slotsLeft = %d, want 2", usage.Invites.SlotsLeft)
	}
	if len(usage.Invites.PendingEmails) != 1 || usage.Invites.PendingEmails[0] != "friend@example.com" {
		t.Fatalf("pendingEmails = %#v", usage.Invites.PendingEmails)
	}
}

func TestQueryOpenAIWhamUsageNormalizesInviteSlotsLeft(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit_reset_credits":{"available_count":0},"invites":{"created":1,"limit":3,"redeemed_count":0,"pending_count":1,"pending_emails":["friend@example.com"]}}`))
	}))
	defer upstream.Close()

	oldUsageURL := openAIWhamUsageURL
	openAIWhamUsageURL = upstream.URL + "/usage"
	t.Cleanup(func() { openAIWhamUsageURL = oldUsageURL })

	svc := &AccountUsageService{}
	usage, _, err := svc.queryOpenAIWhamUsage(context.Background(), &Account{
		ID:       20261,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "access-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	})
	if err != nil {
		t.Fatalf("queryOpenAIWhamUsage() error = %v", err)
	}
	if usage.Invites == nil {
		t.Fatal("expected invites to be passed through")
	}
	if usage.Invites.SlotsLeft != 2 {
		t.Fatalf("slotsLeft = %d, want 2", usage.Invites.SlotsLeft)
	}
	if usage.Invites.PendingCount == nil || *usage.Invites.PendingCount != 1 {
		t.Fatalf("pendingCount = %#v, want 1", usage.Invites.PendingCount)
	}
	if len(usage.Invites.PendingEmails) != 1 || usage.Invites.PendingEmails[0] != "friend@example.com" {
		t.Fatalf("pendingEmails = %#v", usage.Invites.PendingEmails)
	}
}

func TestQueryOpenAIWhamUsageHandlesCamelCaseReferralPayload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"resetCreditsAvailable":2,"invites":{"created":3,"limit":3,"slotsLeft":0,"redeemed":3,"pendingCount":0,"pendingEmails":[],"list":[{"email":"amymills276586@outlook.com","status":"redeemed"},{"email":"mrspamela25971@outlook.com","status":"redeemed"},{"email":"justinsmith925590@outlook.com","status":"redeemed"}]}}`))
	}))
	defer upstream.Close()

	oldUsageURL := openAIWhamUsageURL
	openAIWhamUsageURL = upstream.URL + "/usage"
	t.Cleanup(func() { openAIWhamUsageURL = oldUsageURL })

	svc := &AccountUsageService{}
	usage, updates, err := svc.queryOpenAIWhamUsage(context.Background(), &Account{
		ID:       20262,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "access-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	})
	if err != nil {
		t.Fatalf("queryOpenAIWhamUsage() error = %v", err)
	}
	if usage.RateLimitResetCredits == nil || usage.RateLimitResetCredits.AvailableCount != 2 {
		t.Fatalf("reset credits = %#v, want available_count 2", usage.RateLimitResetCredits)
	}
	if got := updates["codex_reset_credits"]; got != 2 {
		t.Fatalf("codex_reset_credits update = %v, want 2", got)
	}
	if usage.Invites == nil {
		t.Fatal("expected invites to be passed through")
	}
	if usage.Invites.Created != 3 || usage.Invites.Limit != 3 || usage.Invites.SlotsLeft != 0 {
		t.Fatalf("invites = %#v, want created=3 limit=3 slotsLeft=0", usage.Invites)
	}
	if usage.Invites.Redeemed == nil || *usage.Invites.Redeemed != 3 {
		t.Fatalf("redeemed = %#v, want 3", usage.Invites.Redeemed)
	}
}

func TestSendOpenAICodexInviteUsesDefaultReferralEndpoint(t *testing.T) {
	var inviteBody string
	var inviteAccountHeader string
	oldInviteURL := openAICodexInviteURL
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/wham/referrals/invite":
			buf, _ := io.ReadAll(r.Body)
			inviteBody = string(buf)
			inviteAccountHeader = r.Header.Get("Chatgpt-Account-Id")
			if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
				t.Fatalf("Authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"invites":[{"email":"friend@example.com"}]}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	openAICodexInviteURL = upstream.URL + "/backend-api/wham/referrals/invite"
	t.Cleanup(func() {
		openAICodexInviteURL = oldInviteURL
	})
	t.Setenv(openAICodexInviteURLEnv, "")

	svc := &AccountUsageService{accountRepo: &accountUsageStaticAccountRepo{account: &Account{
		ID:       2027,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "access-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}}}

	result, err := svc.SendOpenAICodexInvite(context.Background(), 2027, "friend@example.com")
	if err != nil {
		t.Fatalf("SendOpenAICodexInvite() error = %v", err)
	}
	if result == nil || !result.Sent {
		t.Fatalf("unexpected result %#v", result)
	}
	if !strings.Contains(inviteBody, `"emails":["friend@example.com"]`) || !strings.Contains(inviteBody, `"referral_key":"codex_referral_persistent_invite"`) {
		t.Fatalf("invite body = %s", inviteBody)
	}
	if inviteAccountHeader != "chatgpt-acc" {
		t.Fatalf("Chatgpt-Account-Id = %q, want chatgpt-acc", inviteAccountHeader)
	}
}

func TestSendOpenAICodexInvitePostsReferralPayloadWithoutPreflightUsage(t *testing.T) {
	usageCalls := 0
	var inviteBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/usage":
			usageCalls++
			_, _ = w.Write([]byte(`{"rate_limit_reset_credits":{"available_count":0},"invites":{"slotsLeft":1,"pendingEmails":[]}}`))
		case "/invite":
			buf, _ := io.ReadAll(r.Body)
			inviteBody = string(buf)
			if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
				t.Fatalf("Authorization = %q", got)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	t.Setenv(openAICodexInviteURLEnv, upstream.URL+"/invite")
	t.Setenv(openAICodexInviteReferralKeyEnv, "ref-key")

	svc := &AccountUsageService{accountRepo: &accountUsageStaticAccountRepo{account: &Account{
		ID:       2028,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "access-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}}}

	result, err := svc.SendOpenAICodexInvite(context.Background(), 2028, "Friend@Example.com")
	if err != nil {
		t.Fatalf("SendOpenAICodexInvite() error = %v", err)
	}
	if !strings.Contains(inviteBody, `"emails":["friend@example.com"]`) || !strings.Contains(inviteBody, `"referral_key":"ref-key"`) {
		t.Fatalf("invite body = %s", inviteBody)
	}
	if result == nil || !result.Sent {
		t.Fatalf("unexpected result %#v", result)
	}
	if usageCalls != 0 {
		t.Fatalf("usageCalls = %d, want 0", usageCalls)
	}
}

func TestSendOpenAICodexInviteUsesDefaultReferralKey(t *testing.T) {
	var inviteBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/invite":
			buf, _ := io.ReadAll(r.Body)
			inviteBody = string(buf)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	t.Setenv(openAICodexInviteURLEnv, upstream.URL+"/invite")

	svc := &AccountUsageService{accountRepo: &accountUsageStaticAccountRepo{account: &Account{
		ID:       2029,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "access-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}}}

	result, err := svc.SendOpenAICodexInvite(context.Background(), 2029, "friend@example.com")
	if err != nil {
		t.Fatalf("SendOpenAICodexInvite() error = %v", err)
	}
	if result == nil || !result.Sent {
		t.Fatalf("unexpected result %#v", result)
	}
	if !strings.Contains(inviteBody, `"emails":["friend@example.com"]`) || !strings.Contains(inviteBody, `"referral_key":"codex_referral_persistent_invite"`) {
		t.Fatalf("invite body = %s", inviteBody)
	}
}

func TestBuildOpenAIUsageFromExtraIncludesResetCredits(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	usage := buildOpenAIUsageFromExtra(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_reset_credits": 0,
		},
	}, now)

	if !usage.CodexResetCreditsSupported {
		t.Fatal("expected reset credits to be supported when count exists in extra")
	}
	if usage.CodexResetCreditsAvailableCount == nil || *usage.CodexResetCreditsAvailableCount != 0 {
		t.Fatalf("available count = %v, want 0", usage.CodexResetCreditsAvailableCount)
	}
	if usage.CodexAutoResetMode != openAICodexAutoResetModeOff {
		t.Fatalf("auto reset mode = %q, want off when count is 0", usage.CodexAutoResetMode)
	}
}

func TestNormalizeOpenAICodexAutoResetModeAliases(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"":       openAICodexAutoResetModeOff,
		"off":    openAICodexAutoResetModeOff,
		"short":  openAICodexAutoResetModeShort,
		"5h":     openAICodexAutoResetModeShort,
		"hour":   openAICodexAutoResetModeShort,
		"long":   openAICodexAutoResetModeLong,
		"7d":     openAICodexAutoResetModeLong,
		"week":   openAICodexAutoResetModeLong,
		"month":  openAICodexAutoResetModeLong,
		"random": openAICodexAutoResetModeOff,
	}
	for raw, want := range tests {
		if got := normalizeOpenAICodexAutoResetMode(raw); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSetOpenAICodexAutoResetModeNilExtraFallsBackOff(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{{
			ID:       123,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    nil,
		}}},
		updateExtraCh: make(chan map[string]any, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}

	usage, err := svc.SetOpenAICodexAutoResetMode(context.Background(), 123, openAICodexAutoResetModeShort)
	if err != nil {
		t.Fatalf("SetOpenAICodexAutoResetMode returned error: %v", err)
	}
	if usage.CodexAutoResetMode != openAICodexAutoResetModeOff {
		t.Fatalf("auto reset mode = %q, want off", usage.CodexAutoResetMode)
	}
	select {
	case updates := <-repo.updateExtraCh:
		if updates["codex_auto_reset_mode"] != openAICodexAutoResetModeOff {
			t.Fatalf("persisted mode = %v, want off", updates["codex_auto_reset_mode"])
		}
	case <-time.After(time.Second):
		t.Fatal("expected UpdateExtra to be called")
	}
}

func TestApplyOpenAICodexResetCreditsFromExtraHonorsUnsupportedFlag(t *testing.T) {
	t.Parallel()

	usage := &UsageInfo{}
	applyOpenAICodexResetCreditsFromExtra(usage, map[string]any{
		"codex_reset_credits_supported": false,
		"codex_reset_credits":           3,
	})
	if usage.CodexResetCreditsSupported {
		t.Fatal("expected unsupported flag to block display")
	}
	if usage.CodexResetCreditsAvailableCount != nil {
		t.Fatalf("expected nil count, got %v", usage.CodexResetCreditsAvailableCount)
	}
}

func TestBuildOpenAICodexResetCreditOptimisticUpdates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	updates := buildOpenAICodexResetCreditOptimisticUpdates(&Account{
		Extra: map[string]any{
			"codex_reset_credits_supported": true,
			"codex_reset_credits":           2,
		},
	}, now)
	if got := updates["codex_reset_credits"]; got != 1 {
		t.Fatalf("codex_reset_credits = %v, want 1", got)
	}
	if got := updates["codex_reset_credits_supported"]; got != true {
		t.Fatalf("codex_reset_credits_supported = %v, want true", got)
	}
	if got := updates["codex_reset_credits_updated_at"]; got != now.Format(time.RFC3339) {
		t.Fatalf("updated_at = %v, want %s", got, now.Format(time.RFC3339))
	}

	updates = buildOpenAICodexResetCreditOptimisticUpdates(&Account{
		Extra: map[string]any{
			"codex_reset_credits_supported": false,
			"codex_reset_credits":           2,
		},
	}, now)
	if len(updates) != 0 {
		t.Fatalf("expected no optimistic update for unsupported account, got %v", updates)
	}
}

func TestOpenAICodexResetCreditUpdatesConfirmed(t *testing.T) {
	t.Parallel()

	if openAICodexResetCreditUpdatesConfirmed(nil) {
		t.Fatal("nil updates should not be confirmed")
	}
	if openAICodexResetCreditUpdatesConfirmed(map[string]any{"codex_5h_used_percent": 100}) {
		t.Fatal("usage-only updates should not be confirmed reset credit updates")
	}
	if !openAICodexResetCreditUpdatesConfirmed(map[string]any{"codex_reset_credits": 0}) {
		t.Fatal("presence of codex_reset_credits should confirm reset credit query")
	}
}

func TestBuildCodexUsageProgressFromExtra_ZerosExpiredWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)

	t.Run("expired 5h window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     "2026-03-16T10:00:00Z", // 2h ago
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired window, got %v", progress.Utilization)
		}
		if progress.RemainingSeconds != 0 {
			t.Fatalf("expected RemainingSeconds=0, got %v", progress.RemainingSeconds)
		}
	})

	t.Run("active 5h window keeps utilization", func(t *testing.T) {
		resetAt := now.Add(2 * time.Hour).Format(time.RFC3339)
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     resetAt,
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 42.0 {
			t.Fatalf("expected Utilization=42, got %v", progress.Utilization)
		}
	})

	t.Run("expired 7d window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_7d_used_percent": 88.0,
			"codex_7d_reset_at":     "2026-03-15T00:00:00Z", // yesterday
		}
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired 7d window, got %v", progress.Utilization)
		}
	})
}
