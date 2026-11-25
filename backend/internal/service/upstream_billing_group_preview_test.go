package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type billingGroupPreviewUpstream struct {
	HTTPUpstream
	check func(*http.Request, string, int64) (*http.Response, error)
}

func (u billingGroupPreviewUpstream) DoWithTLS(req *http.Request, proxy string, id int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.check(req, proxy, id)
}

func TestUpstreamBillingGroupPreviewUsesSavedIdentityWithoutUpdatingProbe(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepoStub{account: &Account{
		ID: 7, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://relay.example/v1", "api_key": "saved-key"},
	}}
	groupResponse := strings.TrimSuffix(upstreamBillingProbeSuccessBody, "}") +
		",\"group_name\":\" Example \\n\",\"group_description\":\"Line one\\nLine two\"}"
	var requests []string
	var profiles []HTTPUpstreamProfile
	upstream := billingGroupPreviewUpstream{check: func(req *http.Request, proxy string, id int64) (*http.Response, error) {
		requests = append(requests, req.URL.String())
		profiles = append(profiles, HTTPUpstreamProfileFromContext(req.Context()))
		require.Equal(t, int64(7), id)
		require.Empty(t, proxy)
		require.Equal(t, http.MethodGet, req.Method)
		require.Equal(t, "Bearer saved-key", req.Header.Get("Authorization"))
		require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(groupResponse))}, nil
	}}
	svc := newConcurrentUpstreamBillingProbeService(repo, upstream)

	preview, err := svc.PreviewGroup(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, BillingGroupInfo{Name: "Example", Description: "Line one\nLine two"}, preview)
	require.Nil(t, repo.lastSnapshot)
	require.Nil(t, repo.updatedExtra)
	require.Empty(t, svc.probeSlots)

	snapshot, err := svc.ProbeAccount(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, upstreamBillingProbeDefaultIntervalSeconds, int(snapshot.NextProbeAt.Sub(snapshot.LastAttemptAt).Seconds()))
	require.Equal(t, []string{"https://relay.example/v1/sub2api/billing?include=group", "https://relay.example/v1/sub2api/billing"}, requests)
	require.Equal(t, []HTTPUpstreamProfile{HTTPUpstreamProfileBillingProbe, HTTPUpstreamProfileDefault}, profiles)
	require.NotContains(t, snapshot.Data, "group_name")
}

type billingGroupPreviewAccountRepo struct {
	AccountRepository
	get func(context.Context) (*Account, error)
}

func (r billingGroupPreviewAccountRepo) GetByID(ctx context.Context, _ int64) (*Account, error) {
	return r.get(ctx)
}

func TestUpstreamBillingGroupPreviewDiscardsCanceledWork(t *testing.T) {
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "saved-key", "base_url": "https://relay.example/v1"}}
	for _, cancelAt := range []string{"account load", "upstream reply"} {
		t.Run(cancelAt, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			repo := billingGroupPreviewAccountRepo{get: func(ctx context.Context) (*Account, error) {
				_, hasDeadline := ctx.Deadline()
				require.True(t, hasDeadline)
				if cancelAt == "account load" {
					cancel()
				}
				return account, nil
			}}
			const latePreview = `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_name":"Late preview"}`
			body := &bodyLimitCloseTrackingReadCloser{Reader: strings.NewReader(latePreview)}
			svc := newConcurrentUpstreamBillingProbeService(repo, billingGroupPreviewUpstream{
				check: func(req *http.Request, _ string, _ int64) (*http.Response, error) {
					require.Equal(t, "upstream reply", cancelAt)
					require.NoError(t, req.Context().Err())
					cancel()
					require.ErrorIs(t, req.Context().Err(), context.Canceled)
					return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
				},
			})
			info, err := svc.PreviewGroup(ctx, account.ID)
			require.ErrorIs(t, err, errUpstreamGroupPreviewFailed)
			require.Empty(t, info)
			require.Empty(t, svc.groupPreviewSlots)
			require.Equal(t, cancelAt == "upstream reply", body.closed)
			require.Equal(t, len(latePreview), body.Len())
		})
	}
}

func TestUpstreamBillingGroupPreviewRejectsIneligibleAndBusyRequests(t *testing.T) {
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "saved-key", "base_url": "https://api.openai.com/v1"}}
	repo := &upstreamBillingProbeAccountRepoStub{account: account}
	svc := newConcurrentUpstreamBillingProbeService(repo, billingGroupPreviewUpstream{
		check: func(*http.Request, string, int64) (*http.Response, error) {
			t.Fatal("must not contact the upstream")
			return nil, nil
		},
	})

	for _, target := range []struct{ platform, baseURL string }{
		{PlatformOpenAI, "https://api.openai.com/v1"},
		{PlatformKimi, DefaultKimiPayGBaseURL},
		{PlatformKimi, DefaultKimiCodingBaseURL},
		{PlatformKimi, "https://API.MOONSHOT.CN./v1"},
		{PlatformZhipu, "https://api.z.ai/api/paas/v4"},
	} {
		account.Platform = target.platform
		account.Credentials["base_url"] = target.baseURL
		info, err := svc.PreviewGroup(context.Background(), 7)
		require.NoError(t, err, target.baseURL)
		require.Empty(t, info, target.baseURL)
	}

	account.Type = AccountTypeOAuth
	_, err := svc.PreviewGroup(context.Background(), 7)
	require.ErrorIs(t, err, ErrUpstreamBillingProbeAccountInvalid)

	account.Type = AccountTypeAPIKey
	account.Credentials["base_url"] = "https://relay.example/v1"
	proxyID := int64(8)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{ID: 9}
	_, err = svc.PreviewGroup(context.Background(), 7)
	require.ErrorIs(t, err, errUpstreamGroupPreviewFailed)

	for range cap(svc.groupPreviewSlots) {
		svc.groupPreviewSlots <- struct{}{}
	}
	_, err = svc.PreviewGroup(context.Background(), 7)
	require.ErrorIs(t, err, errUpstreamGroupPreviewBusy)
	require.Empty(t, svc.probeSlots)
}

func TestUpstreamBillingGroupPreviewHandlesOldAndInvalidUpstreams(t *testing.T) {
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "saved-key", "base_url": "https://relay.example:8443/v1",
			credKeyHeaderOverrideEnabled: true,
			credKeyHeaderOverrides:       map[string]any{"x-relay-secret": "custom-relay-key"},
		}, Proxy: &Proxy{Username: "relay-user", Password: "relay-pass"}}
	repo := &upstreamBillingProbeAccountRepoStub{account: account}
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		want      BillingGroupInfo
		wantError error
	}{
		{name: "old endpoint with large error page", status: http.StatusNotFound, body: strings.Repeat("x", upstreamBillingProbeMaxBodyBytes+1)},
		{name: "method not allowed with error page", status: http.StatusMethodNotAllowed, body: "This endpoint only supports POST"},
		{name: "upstream failure with error page", status: http.StatusServiceUnavailable, body: "Upstream unavailable", wantError: errUpstreamGroupPreviewFailed},
		{name: "old response", status: http.StatusOK, body: upstreamBillingProbeSuccessBody},
		{name: "oversized", status: http.StatusOK, body: strings.Repeat("x", upstreamBillingProbeMaxBodyBytes+1), wantError: errUpstreamGroupPreviewFailed},
		{name: "reflected credential", status: http.StatusOK,
			body: `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_name":"saved-key","group_description":"Safe text"}`,
			want: BillingGroupInfo{Description: "Safe text"}},
		{name: "reflected override and hostname", status: http.StatusOK,
			body: `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_name":"Team custom-relay-key","group_description":"Served by RELAY.EXAMPLE"}`},
		{name: "reflected proxy credentials", status: http.StatusOK,
			body: `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_name":"relay-user","group_description":"Password was relay-pass"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.NewReader(tc.body)
			svc := newConcurrentUpstreamBillingProbeService(repo, billingGroupPreviewUpstream{
				check: func(*http.Request, string, int64) (*http.Response, error) {
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(body)}, nil
				},
			})
			info, err := svc.PreviewGroup(context.Background(), account.ID)
			require.ErrorIs(t, err, tc.wantError)
			require.Equal(t, tc.want, info)
			require.Nil(t, repo.lastSnapshot)
			require.Empty(t, svc.groupPreviewSlots)
			if tc.status != http.StatusOK {
				require.Equal(t, len(tc.body), body.Len())
			}
		})
	}
}

func TestUpstreamBillingGroupPreviewValidatesFieldsIndependently(t *testing.T) {
	prefix := `{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token",`
	body := []byte(prefix + `"group_name":"bad\u0000name","group_description":" good\nnotes "}`)
	info, err := parseUpstreamBillingGroupInfo(body)
	require.NoError(t, err)
	require.Equal(t, BillingGroupInfo{Description: "good\nnotes"}, info)

	body = []byte(prefix + `"group_name":"bad` + "\xff" + `name","group_description":"still valid"}`)
	info, err = parseUpstreamBillingGroupInfo(body)
	require.NoError(t, err)
	require.Equal(t, BillingGroupInfo{Description: "still valid"}, info)

	body = []byte(prefix + `"group_name":"bad\u202ename","group_description":"still valid"}`)
	info, err = parseUpstreamBillingGroupInfo(body)
	require.NoError(t, err)
	require.Equal(t, BillingGroupInfo{Description: "still valid"}, info)

	_, err = parseUpstreamBillingGroupInfo([]byte(`{"group_name":"no billing envelope"}`))
	require.ErrorIs(t, err, errUpstreamGroupPreviewFailed)
}
