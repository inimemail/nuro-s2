package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type failingCodexIdentitySettingRepo struct {
	SettingRepository
}

func (failingCodexIdentitySettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, errors.New("settings unavailable")
}

func TestStripOpenAILegacyResponsesBeta(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   []string
	}{
		{name: "legacy only", values: []string{"responses=experimental"}},
		{name: "case insensitive legacy", values: []string{"RESPONSES=EXPERIMENTAL"}},
		{name: "preserves independent token", values: []string{"responses=experimental, assistants=v2"}, want: []string{"assistants=v2"}},
		{name: "preserves multiple header lines", values: []string{"feature-a", "responses=experimental, feature-b"}, want: []string{"feature-a", "feature-b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := make(http.Header)
			for _, value := range tt.values {
				headers.Add("OpenAI-Beta", value)
			}

			stripOpenAILegacyResponsesBeta(headers)

			require.Equal(t, tt.want, headers.Values("OpenAI-Beta"))
		})
	}
}

func TestCodexCanonicalAuthIdentity(t *testing.T) {
	userAgent, originator := CodexCanonicalAuthIdentity()

	require.NotEmpty(t, userAgent)
	require.Equal(t, "codex_cli_rs", originator)
}

func TestRefreshOpenAICodexIdentityRuntimeAppliesConfigWithoutRepository(t *testing.T) {
	t.Cleanup(func() { publishCodexIdentityRuntime("", "", "", true) })

	publishCodexIdentityRuntime("0.151.0", "", "", false)
	enabled := NewSettingService(nil, &config.Config{})
	require.NoError(t, enabled.RefreshOpenAICodexIdentityRuntime(t.Context()))
	require.True(t, currentCodexIdentityRuntime().enforceIdentity)
	require.Equal(t, "0.151.0", currentCodexIdentityRuntime().version)

	disabled := NewSettingService(nil, &config.Config{Gateway: config.GatewayConfig{
		DisableCodexIdentityEnforcement: true,
	}})
	require.NoError(t, disabled.RefreshOpenAICodexIdentityRuntime(t.Context()))
	require.False(t, currentCodexIdentityRuntime().enforceIdentity)
	require.Equal(t, "0.151.0", currentCodexIdentityRuntime().version)
}

func TestRefreshOpenAICodexIdentityRuntimeKeepsIdentityWhenRepositoryFails(t *testing.T) {
	t.Cleanup(func() { publishCodexIdentityRuntime("", "", "", true) })

	publishCodexIdentityRuntime("0.151.0", "", "", false)
	svc := NewSettingService(failingCodexIdentitySettingRepo{}, &config.Config{})
	err := svc.RefreshOpenAICodexIdentityRuntime(t.Context())

	require.ErrorContains(t, err, "settings unavailable")
	require.True(t, currentCodexIdentityRuntime().enforceIdentity)
	require.Equal(t, "0.151.0", currentCodexIdentityRuntime().version)
}

func TestEnforceCodexIdentityHeaders(t *testing.T) {
	t.Cleanup(func() { publishCodexIdentityRuntime("", "", "", true) })
	publishCodexIdentityRuntime("", "", "", false)

	const tuiUA = "codex-tui/0.140.2 (Mac OS X 14.0; arm64) iTerm (codex-tui; 0.140.2)"

	tests := []struct {
		name           string
		originator     string
		userAgent      string
		version        string
		wantOriginator string
		wantUA         string
		wantVersion    string
	}{
		{
			name:           "错配 originator 按最终 UA 重配",
			originator:     "codex_cli_rs",
			userAgent:      tuiUA,
			wantOriginator: "codex-tui",
			wantUA:         tuiUA,
		},
		{
			name:           "官方配套身份原样保留",
			originator:     "codex-tui",
			userAgent:      tuiUA,
			wantOriginator: "codex-tui",
			wantUA:         tuiUA,
		},
		{
			name:           "第三方 UA 整体回退默认身份",
			originator:     "opencode",
			userAgent:      "luna/1.0.0",
			wantOriginator: "codex_cli_rs",
			wantUA:         codexCLIUserAgent,
		},
		{
			name:           "UA 缺失回退默认身份",
			originator:     "codex_vscode",
			wantOriginator: "codex_cli_rs",
			wantUA:         codexCLIUserAgent,
		},
		{
			name:           "originator override UA 首段被尾部真实身份重写",
			originator:     "cccc",
			userAgent:      "cccc/0.142.0 (Ubuntu 22.4.0; x86_64) screen (codex-tui; 0.142.0)",
			wantOriginator: "codex-tui",
			wantUA:         "codex-tui/0.142.0 (Ubuntu 22.4.0; x86_64) screen (codex-tui; 0.142.0)",
		},
		{
			name:           "低于门槛的 version 提升为内置版本",
			originator:     "codex_cli_rs",
			userAgent:      "codex_cli_rs/0.125.0",
			version:        "0.125.0",
			wantOriginator: "codex_cli_rs",
			wantUA:         "codex_cli_rs/0.125.0",
			wantVersion:    codexCLIVersion,
		},
		{
			name:           "达标 version 原样保留",
			originator:     "codex_cli_rs",
			userAgent:      "codex_cli_rs/0.145.0",
			version:        "0.145.0",
			wantOriginator: "codex_cli_rs",
			wantUA:         "codex_cli_rs/0.145.0",
			wantVersion:    "0.145.0",
		},
		{
			name:           "未携带 version 不注入",
			originator:     "codex_cli_rs",
			userAgent:      "codex_cli_rs/0.98.0",
			wantOriginator: "codex_cli_rs",
			wantUA:         "codex_cli_rs/0.98.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := make(http.Header)
			if tt.originator != "" {
				h.Set("originator", tt.originator)
			}
			if tt.userAgent != "" {
				h.Set("user-agent", tt.userAgent)
			}
			if tt.version != "" {
				h.Set("version", tt.version)
			}

			enforceCodexIdentityHeaders(h)

			require.Equal(t, tt.wantOriginator, h.Get("originator"))
			require.Equal(t, tt.wantUA, h.Get("user-agent"))
			require.Equal(t, tt.wantVersion, h.Get("version"))
		})
	}
}

// compat messages bridge 故意不带 originator：收口必须保持 no-op，不得注入身份头。
func TestEnforceCodexIdentityHeaders_NoOriginatorIsNoop(t *testing.T) {
	t.Cleanup(func() { publishCodexIdentityRuntime("", "", "", true) })
	publishCodexIdentityRuntime("", "", "", false)

	h := make(http.Header)
	h.Set("user-agent", "luna/1.0.0")

	enforceCodexIdentityHeaders(h)

	require.Empty(t, h.Get("originator"))
	require.Equal(t, "luna/1.0.0", h.Get("user-agent"))
}
