package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOllamaCloudUsageRefreshWithoutEncryptorFailsClosed(t *testing.T) {
	svc := &OllamaCloudUsageService{}
	_, err := svc.refreshAccount(context.Background(), 1, ollamaCloudDefaultIntervalMinutes)
	require.ErrorIs(t, err, ErrOllamaCloudUsageEncryptionKey)
}

func TestOllamaCloudUsageEligibilityAndCookieSanitization(t *testing.T) {
	base := map[string]any{"base_url": "https://ollama.com/v1", "api_key": "ollama-key"}
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: base}
	require.True(t, IsOllamaCloudUsageAccount(a))
	a.Platform = PlatformGrok
	require.False(t, IsOllamaCloudUsageAccount(a))

	for _, raw := range []string{"", "session=abc\r\nX-Leak: 1", "session=abc; Path=/"} {
		_, err := normalizeOllamaCloudCookie(raw)
		require.Error(t, err)
	}
	require.Equal(t, "session=abc; other=def", mustOllamaCookie(t, "session=abc; other=def"))
}

func TestParseOllamaCloudUsageHTMLDoesNotRetainHTML(t *testing.T) {
	data := parseOllamaCloudUsageHTML(`<title>Plan Pro</title><div>Usage 12.5%</div><div>Used 88%</div><script>session=secret</script>`)
	require.NotNil(t, data)
	require.NotNil(t, data.FiveHour)
	require.Equal(t, 12.5, data.FiveHour.UsedPercent)
	require.NotContains(t, data.Plan, "<")
}

func TestOllamaCloudStateNeverReturnsSession(t *testing.T) {
	a := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://ollama.com"}, Extra: map[string]any{
		OllamaCloudUsageSessionExtraKey:     "ciphertext",
		OllamaCloudUsageAutoRefreshExtraKey: true,
		OllamaCloudUsageSnapshotExtraKey:    map[string]any{"status": "ok", "next_refresh_at": time.Now().UTC()},
	}}
	state := ollamaCloudState(a, true)
	require.True(t, state.Configured)
	require.NotNil(t, state.Snapshot)
}

func TestOllamaCloudUsageAutoRefreshDueAt(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	fetchedAt := now.Add(-20 * time.Minute)
	lastUsedAt := now.Add(-30 * time.Second)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
		Credentials: map[string]any{
			"base_url": "https://ollama.com/v1",
			"api_key":  "shared-key",
		},
		Extra: map[string]any{
			OllamaCloudUsageSessionExtraKey:     "ciphertext",
			OllamaCloudUsageAutoRefreshExtraKey: true,
			OllamaCloudUsageSnapshotExtraKey: OllamaCloudUsageSnapshot{
				Status: "ok", FetchedAt: &fetchedAt, LastAttemptAt: fetchedAt,
			},
		},
		LastUsedAt: &lastUsedAt,
	}
	dueAt, due := ollamaCloudUsageAutoRefreshDueAt(now, account, time.Minute, time.Hour)
	require.True(t, due)
	require.Equal(t, lastUsedAt.Add(time.Minute), dueAt)

	recentFetch := now.Add(-5 * time.Minute)
	account.Extra[OllamaCloudUsageSnapshotExtraKey] = OllamaCloudUsageSnapshot{Status: "ok", FetchedAt: &recentFetch, LastAttemptAt: recentFetch}
	dueAt, due = ollamaCloudUsageAutoRefreshDueAt(now, account, time.Minute, time.Hour)
	require.True(t, due)
	require.Equal(t, recentFetch.Add(OllamaCloudUsageMinFetchInterval), dueAt)

	account.LastUsedAt = &recentFetch
	_, due = ollamaCloudUsageAutoRefreshDueAt(now, account, time.Minute, time.Hour)
	require.False(t, due)
}

func mustOllamaCookie(t *testing.T, raw string) string {
	t.Helper()
	got, err := normalizeOllamaCloudCookie(raw)
	require.NoError(t, err)
	return got
}
