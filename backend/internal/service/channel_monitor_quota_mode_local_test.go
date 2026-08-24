package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

type channelMonitorDecryptErrorEncryptor struct{}

func (channelMonitorDecryptErrorEncryptor) Encrypt(string) (string, error) { return "ciphertext", nil }
func (channelMonitorDecryptErrorEncryptor) Decrypt(string) (string, error) {
	return "", errors.New("key changed")
}

type channelMonitorRoundtripEncryptor struct{}

func (channelMonitorRoundtripEncryptor) Encrypt(value string) (string, error) {
	return "OLD:" + value, nil
}
func (channelMonitorRoundtripEncryptor) Decrypt(value string) (string, error) {
	return strings.TrimPrefix(value, "OLD:"), nil
}

type channelMonitorUpdateRepoStub struct {
	ChannelMonitorRepository
	monitor *ChannelMonitor
}

func (r *channelMonitorUpdateRepoStub) GetByID(context.Context, int64) (*ChannelMonitor, error) {
	copy := *r.monitor
	return &copy, nil
}
func (r *channelMonitorUpdateRepoStub) Update(_ context.Context, m *ChannelMonitor) error {
	r.monitor = m
	return nil
}

func TestChannelMonitorQuotaCreateValidation(t *testing.T) {
	accountID := int64(42)
	require.NoError(t, validateCreateParams(ChannelMonitorCreateParams{
		Provider: MonitorProviderKimi, CheckMode: MonitorCheckModeQuota,
		AccountID: &accountID, IntervalSeconds: 60,
	}))
	require.ErrorIs(t, validateCreateParams(ChannelMonitorCreateParams{
		Provider: MonitorProviderKimi, CheckMode: MonitorCheckModeQuota,
		IntervalSeconds: 60,
	}), ErrChannelMonitorAccountRequired)
	require.ErrorIs(t, validateCreateParams(ChannelMonitorCreateParams{
		Provider: MonitorProviderAntigravity, CheckMode: MonitorCheckModeQuotaProbe,
		AccountID: &accountID, Endpoint: "https://example.com", APIKey: "key",
		PrimaryModel: "model", IntervalSeconds: 60,
	}), ErrChannelMonitorInvalidCheckMode)
	require.Equal(t, MonitorDefaultQuotaModel, normalizeMonitorPrimaryModel(
		MonitorProviderDeepSeek, MonitorCheckModeQuota, "",
	))
}

func TestChannelMonitorQuotaCapabilityMatrix(t *testing.T) {
	require.NoError(t, monitorAccountQuotaCapability(&Account{
		Platform: PlatformKimi, Type: AccountTypeAPIKey,
		Extra: map[string]any{cnBillingModeExtraKey: CNBillingModeCodingPlan},
	}))
	require.ErrorIs(t, monitorAccountQuotaCapability(&Account{
		Platform: PlatformDeepSeek, Type: AccountTypeAPIKey,
		Extra: map[string]any{cnBillingModeExtraKey: CNBillingModeCodingPlan},
	}), ErrChannelMonitorAccountNotSupportable)
	require.ErrorIs(t, monitorAccountQuotaCapability(&Account{
		Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Extra: map[string]any{cnBillingModeExtraKey: CNBillingModePayG},
	}), ErrChannelMonitorAccountNotSupportable)
	require.ErrorIs(t, monitorAccountQuotaCapability(&Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
	}), ErrChannelMonitorAccountNotSupportable)
	require.NoError(t, monitorAccountQuotaCapability(&Account{
		Platform: PlatformAnthropic, Type: AccountTypeSetupToken,
	}))
	require.ErrorIs(t, monitorAccountQuotaCapability(&Account{
		Platform: PlatformAntigravity, Type: AccountTypeAPIKey,
	}), ErrChannelMonitorAccountNotSupportable)
	require.NoError(t, monitorAccountQuotaCapability(&Account{
		Platform: PlatformAntigravity, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "token"},
	}))
}

func TestDeriveQuotaCheckResult(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		snapshot *domain.MonitorQuotaSnapshot
		status   string
	}{
		{name: "healthy", snapshot: &domain.MonitorQuotaSnapshot{Success: true, Tiers: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 20}}}, status: MonitorStatusOperational},
		{name: "high usage", snapshot: &domain.MonitorQuotaSnapshot{Success: true, Tiers: []domain.MonitorQuotaTier{{Window: "weekly", UsedPercent: 91}}}, status: MonitorStatusDegraded},
		{name: "low balance", snapshot: &domain.MonitorQuotaSnapshot{Success: true, BalanceLow: true}, status: MonitorStatusDegraded},
		{name: "credentials", snapshot: &domain.MonitorQuotaSnapshot{CredentialInvalid: true, Error: "401", Success: false}, status: MonitorStatusFailed},
		{name: "upstream error", snapshot: &domain.MonitorQuotaSnapshot{Error: "timeout", Success: false}, status: MonitorStatusError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.status, deriveQuotaCheckResult(tc.snapshot, "quota", now).Status)
		})
	}
}

func TestMonitorQuotaSnapshotFromUsageDoesNotHideDegradedProviderState(t *testing.T) {
	now := time.Now()
	unknown := monitorQuotaSnapshotFromUsage(&UsageInfo{ErrorCode: "quota_unknown", Error: "No quota snapshot"}, now)
	require.NotNil(t, unknown)
	require.False(t, unknown.Success)
	require.False(t, unknown.CredentialInvalid)

	unauthenticated := monitorQuotaSnapshotFromUsage(&UsageInfo{ErrorCode: "unauthenticated"}, now)
	require.NotNil(t, unauthenticated)
	require.True(t, unauthenticated.CredentialInvalid)

	require.Nil(t, monitorQuotaSnapshotFromUsage(&UsageInfo{FiveHour: &UsageProgress{Utilization: 10}}, now))
}

func TestAttachQuotaSnapshotCombinesQuotaHealth(t *testing.T) {
	now := time.Now()
	results := []*CheckResult{{Model: "model", Status: MonitorStatusOperational, CheckedAt: now}}
	attachQuotaSnapshot(results, &domain.MonitorQuotaSnapshot{Success: false, Error: "timeout", FetchedAt: now})
	require.Equal(t, MonitorStatusError, results[0].Status)
	require.Contains(t, results[0].Message, "timeout")

	results = []*CheckResult{{Model: "model", Status: MonitorStatusOperational, CheckedAt: now}}
	attachQuotaSnapshot(results, &domain.MonitorQuotaSnapshot{Success: true, Tiers: []domain.MonitorQuotaTier{{Window: "5h", UsedPercent: 95}}, FetchedAt: now})
	require.Equal(t, MonitorStatusDegraded, results[0].Status)
}

func TestValidateProbeAPIKeyFailsClosedWhenStoredKeyCannotDecrypt(t *testing.T) {
	svc := NewChannelMonitorService(nil, channelMonitorDecryptErrorEncryptor{})
	err := svc.validateProbeAPIKey(&ChannelMonitor{
		CheckMode: MonitorCheckModeProbe,
		APIKey:    "ciphertext",
	}, "")
	require.ErrorIs(t, err, ErrChannelMonitorAPIKeyDecryptFailed)
}

func TestChannelMonitorUpdateRejectsProviderSwitchWithoutFreshProbeKey(t *testing.T) {
	repo := &channelMonitorUpdateRepoStub{monitor: &ChannelMonitor{
		ID: 1, Provider: MonitorProviderOpenAI, CheckMode: MonitorCheckModeProbe,
		Endpoint: "https://old.example.com", APIKey: "ciphertext", PrimaryModel: "gpt-5.4",
		IntervalSeconds: 60,
	}}
	svc := NewChannelMonitorService(repo, channelMonitorRoundtripEncryptor{})
	provider := MonitorProviderKimi
	err := func() error {
		_, err := svc.Update(context.Background(), 1, ChannelMonitorUpdateParams{Provider: &provider})
		return err
	}()
	require.ErrorIs(t, err, ErrChannelMonitorMissingAPIKey)
}
