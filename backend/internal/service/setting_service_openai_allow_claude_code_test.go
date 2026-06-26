package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type allowClaudeCodeSettingRepoStub struct{ values map[string]string }

func (s *allowClaudeCodeSettingRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unused")
}
func (s *allowClaudeCodeSettingRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return "", ErrSettingNotFound
}
func (s *allowClaudeCodeSettingRepoStub) Set(ctx context.Context, key, value string) error {
	panic("unused")
}
func (s *allowClaudeCodeSettingRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		if v, ok := s.values[key]; ok {
			out[key] = v
		}
	}
	return out, nil
}
func (s *allowClaudeCodeSettingRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	panic("unused")
}
func (s *allowClaudeCodeSettingRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	panic("unused")
}
func (s *allowClaudeCodeSettingRepoStub) Delete(ctx context.Context, key string) error {
	panic("unused")
}

func TestSettingService_IsOpenAIAllowClaudeCodeCodexPluginEnabled(t *testing.T) {
	t.Run("默认关闭（设置缺失）", func(t *testing.T) {
		svc := NewSettingService(&allowClaudeCodeSettingRepoStub{values: map[string]string{}}, &config.Config{})
		require.False(t, svc.IsOpenAIAllowClaudeCodeCodexPluginEnabled(context.Background()))
	})
	t.Run("值为 true 时开启", func(t *testing.T) {
		svc := NewSettingService(&allowClaudeCodeSettingRepoStub{values: map[string]string{
			SettingKeyOpenAIAllowClaudeCodeCodexPlugin: "true",
		}}, &config.Config{})
		require.True(t, svc.IsOpenAIAllowClaudeCodeCodexPluginEnabled(context.Background()))
	})
	t.Run("值非 true 时关闭", func(t *testing.T) {
		svc := NewSettingService(&allowClaudeCodeSettingRepoStub{values: map[string]string{
			SettingKeyOpenAIAllowClaudeCodeCodexPlugin: "false",
		}}, &config.Config{})
		require.False(t, svc.IsOpenAIAllowClaudeCodeCodexPluginEnabled(context.Background()))
	})
}

func TestSettingService_GetCodexCLIOnlyPolicy(t *testing.T) {
	svc := NewSettingService(&allowClaudeCodeSettingRepoStub{values: map[string]string{
		SettingKeyCodexCLIOnlyBlacklist:                `[{"ua_contains":["BadBot/"]}]`,
		SettingKeyCodexCLIOnlyWhitelist:                `[{"originator":"Trusted Tool","ua_contains":["Trusted Tool/"],"skip_engine_fingerprint":true},{"originator":"Unsafe"}]`,
		SettingKeyCodexCLIOnlyAllowAppServerClients:    "true",
		SettingKeyCodexCLIOnlyEngineFingerprintSignals: `[{"type":"header_exact","match":["x-codex-required"],"required":true}]`,
	}}, &config.Config{})

	policy := svc.GetCodexCLIOnlyPolicy(context.Background())
	require.Len(t, policy.Blacklist, 1)
	require.Equal(t, []string{"BadBot/"}, policy.Blacklist[0].UAContains)
	require.Len(t, policy.Whitelist, 1)
	require.Equal(t, "Trusted Tool", policy.Whitelist[0].Originator)
	require.True(t, policy.Whitelist[0].SkipEngineFingerprint)
	require.True(t, policy.AllowAppServerClients)
	require.Len(t, policy.EngineFingerprintSignals, 1)
	require.Equal(t, "header_exact", policy.EngineFingerprintSignals[0].Type)
}

func TestSettingService_GetCodexCLIOnlyPolicy_InvalidJSONFailsOpen(t *testing.T) {
	svc := NewSettingService(&allowClaudeCodeSettingRepoStub{values: map[string]string{
		SettingKeyCodexCLIOnlyBlacklist:                `not json`,
		SettingKeyCodexCLIOnlyWhitelist:                `[{"originator":"Unsafe"}]`,
		SettingKeyCodexCLIOnlyEngineFingerprintSignals: `not json`,
	}}, &config.Config{})

	policy := svc.GetCodexCLIOnlyPolicy(context.Background())
	require.Empty(t, policy.Blacklist)
	require.Empty(t, policy.Whitelist)
	require.Empty(t, policy.EngineFingerprintSignals)
	require.False(t, policy.AllowAppServerClients)
}
