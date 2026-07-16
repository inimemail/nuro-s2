package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

type upstreamBillingSettingRepo struct{ values map[string]string }

func (r *upstreamBillingSettingRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}
func (r *upstreamBillingSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}
func (r *upstreamBillingSettingRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}
func (r *upstreamBillingSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (r *upstreamBillingSettingRepo) SetMultiple(context.Context, map[string]string) error {
	return nil
}
func (r *upstreamBillingSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, nil
}
func (r *upstreamBillingSettingRepo) Delete(_ context.Context, key string) error {
	delete(r.values, key)
	return nil
}

func TestUpstreamBillingProbeSettingsDefaultDisabledAndPersisted(t *testing.T) {
	repo := &upstreamBillingSettingRepo{values: make(map[string]string)}
	svc := &SettingService{settingRepo: repo}
	settings, err := svc.GetUpstreamBillingProbeSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 30, settings.IntervalMinutes)

	require.NoError(t, svc.SetUpstreamBillingProbeSettings(context.Background(), &UpstreamBillingProbeSettings{Enabled: true, IntervalMinutes: 15}))
	var stored UpstreamBillingProbeSettings
	require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyUpstreamBillingProbeSettings]), &stored))
	require.True(t, stored.Enabled)
	require.Equal(t, 15, stored.IntervalMinutes)
}

func TestParseUpstreamBillingProbeResponseSanitizesAndValidates(t *testing.T) {
	body := []byte(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":1.2,"resolved_rate_multiplier":1.2,"peak_rate_enabled":false,"effective_rate_multiplier":1.2,"observed_at":"2026-07-16T04:00:00Z","ignored_secret":"do-not-store"}`)
	data, err := parseUpstreamBillingProbeResponse(body)
	require.NoError(t, err)
	require.Equal(t, 1.2, data["effective_rate_multiplier"])
	require.NotContains(t, data, "ignored_secret")

	bad := []byte(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":1.2,"resolved_rate_multiplier":1.2,"peak_rate_enabled":false,"effective_rate_multiplier":9,"observed_at":"2026-07-16T04:00:00Z"}`)
	_, err = parseUpstreamBillingProbeResponse(bad)
	require.Error(t, err)
}

func TestSafeProbeErrorDoesNotExposeInternalDetails(t *testing.T) {
	require.Equal(t, "probe_failed", safeProbeError(context.DeadlineExceeded))
}
