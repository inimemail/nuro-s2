package websearch

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type exaFixtureTransport func(*http.Request) (*http.Response, error)

func (fixture exaFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return fixture(req)
}

func TestExaSearchTransport(t *testing.T) {
	client := &http.Client{Transport: exaFixtureTransport(func(req *http.Request) (*http.Response, error) {
		require.Equal(t, exaSearchEndpoint, req.URL.String())
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "fixture-key", req.Header.Get("X-Api-Key"))
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"query":"local fixture","numResults":1,"contents":{"highlights":true}}`, string(body))
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
			`{"results":[{"url":"https://example.org/","title":"Example","highlights":["One","Two"],"publishedDate":"2026-10-01"},{"url":"https://example.net/"}]}`,
		))}, nil
	})}
	provider := NewExaProvider("fixture-key", client)
	result, err := provider.Search(context.Background(), SearchRequest{Query: "local fixture", MaxResults: 1})
	require.NoError(t, err)
	require.Equal(t, []SearchResult{{URL: "https://example.org/", Title: "Example", Snippet: "One\nTwo", PageAge: "2026-10-01"}}, result.Results)
}

func TestSearchProvidersKeepRequestsOnConfiguredEndpoint(t *testing.T) {
	for _, entry := range []struct {
		name     string
		endpoint string
		create   func(*http.Client) Provider
	}{
		{"exa", exaSearchEndpoint, func(client *http.Client) Provider { return NewExaProvider("fixture", client) }},
		{"brave", braveSearchEndpoint, func(client *http.Client) Provider { return NewBraveProvider("fixture", client) }},
		{"tavily", tavilySearchEndpoint, func(client *http.Client) Provider { return NewTavilyProvider("fixture", client) }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: exaFixtureTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, entry.endpoint, req.URL.Scheme+"://"+req.URL.Host+req.URL.Path)
				return &http.Response{
					StatusCode: http.StatusTemporaryRedirect,
					Header:     http.Header{"Location": {"/search-status"}},
					Body:       http.NoBody,
				}, nil
			})}
			_, err := entry.create(client).Search(t.Context(), SearchRequest{Query: "local fixture"})
			require.ErrorContains(t, err, "307")
			require.Equal(t, 1, calls)
			require.Nil(t, client.CheckRedirect)
		})
	}
}

func TestExaProviderPreservesProxyFailureWithoutExposingDetails(t *testing.T) {
	client := &http.Client{Transport: exaFixtureTransport(func(*http.Request) (*http.Response, error) {
		return nil, &net.DNSError{Err: "no such host", Name: "private.invalid"}
	})}
	proxy := "http://fixture.invalid:3128"
	manager := NewManager([]ProviderConfig{{Type: ProviderTypeExa, APIKey: "fixture"}}, nil)
	manager.clientCache[proxy] = client
	_, _, err := manager.SearchWithBestProvider(t.Context(), SearchRequest{Query: "fixture", ProxyURL: proxy})
	require.ErrorIs(t, err, ErrProxyUnavailable)
	require.NotContains(t, err.Error(), "private.invalid")
}

func TestExaCanceledSearchReleasesReservedQuota(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	manager := NewManager([]ProviderConfig{{Type: ProviderTypeExa, APIKey: "fixture", QuotaLimit: 1}}, client)
	manager.clientCache[""] = &http.Client{Transport: exaFixtureTransport(func(*http.Request) (*http.Response, error) {
		cancel()
		return nil, context.Canceled
	})}
	_, _, err := manager.SearchWithBestProvider(ctx, SearchRequest{Query: "fixture"})
	require.ErrorIs(t, err, context.Canceled)
	used, err := manager.GetUsage(t.Context(), ProviderTypeExa)
	require.NoError(t, err)
	require.Zero(t, used)
}

func TestWebSearchQuotaRollbackUsesReservationWindow(t *testing.T) {
	for _, reset := range []string{"explicit reset", "expiry"} {
		t.Run(reset, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = client.Close() })
			ctx := t.Context()
			cfg := ProviderConfig{Type: ProviderTypeExa, QuotaLimit: 1}
			manager := NewManager([]ProviderConfig{cfg}, client)

			allowed, old := manager.tryReserveQuota(ctx, cfg)
			require.True(t, allowed)
			require.NotEmpty(t, old)
			allowed, reservation := manager.tryReserveQuota(ctx, cfg)
			require.False(t, allowed)
			require.Empty(t, reservation)
			used, err := manager.GetUsage(ctx, cfg.Type)
			require.NoError(t, err)
			require.EqualValues(t, 1, used)

			if reset == "expiry" {
				server.FastForward(defaultQuotaTTL)
			} else {
				require.NoError(t, manager.ResetUsage(ctx, cfg.Type))
			}
			manager.rollbackQuota(ctx, cfg, old)
			require.False(t, server.Exists(quotaRedisKey(cfg.Type)))

			allowed, current := manager.tryReserveQuota(ctx, cfg)
			require.True(t, allowed)
			require.NotEmpty(t, current)
			require.NotEqual(t, old, current)
			manager.rollbackQuota(ctx, cfg, old)
			used, err = manager.GetUsage(ctx, cfg.Type)
			require.NoError(t, err)
			require.EqualValues(t, 1, used)
			manager.rollbackQuota(ctx, cfg, current)
			used, err = manager.GetUsage(ctx, cfg.Type)
			require.NoError(t, err)
			require.Zero(t, used)
		})
	}
}
