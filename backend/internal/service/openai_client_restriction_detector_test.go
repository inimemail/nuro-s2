package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newCodexDetectorTestContext(ua string, originator string) *gin.Context {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if ua != "" {
		c.Request.Header.Set("User-Agent", ua)
	}
	if originator != "" {
		c.Request.Header.Set("originator", originator)
	}
	return c
}

func TestOpenAICodexClientRestrictionDetector_Detect(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("未开启开关时绕过", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}}

		result := detector.Detect(newCodexDetectorTestContext("curl/8.0", ""), account, nil)
		require.False(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonDisabled, result.Reason)
	})

	t.Run("开启后 codex_cli_rs 命中", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}

		result := detector.Detect(newCodexDetectorTestContext("codex_cli_rs/0.99.0", ""), account, nil)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedUA, result.Reason)
	})

	t.Run("开启后 codex_vscode 命中", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}

		result := detector.Detect(newCodexDetectorTestContext("codex_vscode/1.0.0", ""), account, nil)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedUA, result.Reason)
	})

	t.Run("开启后 codex_app 命中", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}

		result := detector.Detect(newCodexDetectorTestContext("codex_app/2.1.0", ""), account, nil)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedUA, result.Reason)
	})

	t.Run("开启后 originator 命中", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}

		result := detector.Detect(newCodexDetectorTestContext("curl/8.0", "codex_chatgpt_desktop"), account, nil)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedOriginator, result.Reason)
	})

	t.Run("开启后非官方客户端拒绝", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}

		result := detector.Detect(newCodexDetectorTestContext("curl/8.0", "my_client"), account, nil)
		require.True(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonNotMatchedUA, result.Reason)
	})

	t.Run("开启 ForceCodexCLI 时允许通过", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(&config.Config{
			Gateway: config.GatewayConfig{ForceCodexCLI: true},
		})
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}

		result := detector.Detect(newCodexDetectorTestContext("curl/8.0", "my_client"), account, nil)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonForceCodexCLI, result.Reason)
	})
}

func TestOpenAICodexClientRestrictionDetector_Detect_AllowedClients(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		claudeCodeUA         = "Claude Code/0.5.0 (Macos 15.5; arm64) iTerm2.app (Claude Code; 1.0.4)"
		claudeCodeOriginator = "Claude Code"
	)

	t.Run("配置 claude_code 白名单且命中真实签名时放行", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				"codex_cli_only":                 true,
				"codex_cli_only_allowed_clients": []any{"claude_code"},
			},
		}

		result := detector.Detect(newCodexDetectorTestContext(claudeCodeUA, claudeCodeOriginator), account, nil)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedAllowedClient, result.Reason)
	})

	t.Run("配置白名单但伪造 originator 仍拒绝", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				"codex_cli_only":                 true,
				"codex_cli_only_allowed_clients": []any{"claude_code"},
			},
		}

		result := detector.Detect(newCodexDetectorTestContext(claudeCodeUA, "my_client"), account, nil)
		require.True(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonNotMatchedUA, result.Reason)
	})

	t.Run("未配置白名单时 Claude Code 签名仍拒绝", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}

		result := detector.Detect(newCodexDetectorTestContext(claudeCodeUA, claudeCodeOriginator), account, nil)
		require.True(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonNotMatchedUA, result.Reason)
	})

	t.Run("未开启 codex_cli_only 时白名单不参与，直接绕过", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only_allowed_clients": []any{"claude_code"}},
		}

		result := detector.Detect(newCodexDetectorTestContext(claudeCodeUA, claudeCodeOriginator), account, nil)
		require.False(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonDisabled, result.Reason)
	})

	t.Run("全局列表含 claude_code + 命中签名 → 放行(global)", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}
		result := detector.Detect(
			newCodexDetectorTestContext("Claude Code/0.5.0 (Macos 15.5; arm64) iTerm2.app (Claude Code; 1.0.4)", "Claude Code"),
			account,
			[]string{"claude_code"},
		)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedGlobalAllowedClient, result.Reason)
	})

	t.Run("全局列表含 claude_code + 非签名 → 403", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}
		result := detector.Detect(newCodexDetectorTestContext("curl/8.0", "my_client"), account, []string{"claude_code"})
		require.True(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonNotMatchedUA, result.Reason)
	})

	t.Run("全局列表为空 + 账号未配 → 403", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{"codex_cli_only": true},
		}
		result := detector.Detect(
			newCodexDetectorTestContext("Claude Code/0.5.0 (Macos) (Claude Code; 1.0.4)", "Claude Code"),
			account,
			nil,
		)
		require.True(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonNotMatchedUA, result.Reason)
	})

	t.Run("账号白名单优先于全局列表（reason=account）", func(t *testing.T) {
		detector := NewOpenAICodexClientRestrictionDetector(nil)
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				"codex_cli_only":                 true,
				"codex_cli_only_allowed_clients": []any{"claude_code"},
			},
		}
		result := detector.Detect(
			newCodexDetectorTestContext("Claude Code/0.5.0 (Macos) (Claude Code; 1.0.4)", "Claude Code"),
			account,
			[]string{"claude_code"},
		)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedAllowedClient, result.Reason)
	})
}

func TestOpenAICodexClientRestrictionDetector_DetectWithPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"codex_cli_only": true},
	}
	detector := NewOpenAICodexClientRestrictionDetector(nil)

	t.Run("空策略保持旧官方客户端放行", func(t *testing.T) {
		result := detector.DetectWithPolicy(newCodexDetectorTestContext("codex-tui/0.125.0", ""), account, nil, CodexCLIOnlyPolicy{}, nil)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedUA, result.Reason)
	})

	t.Run("黑名单优先阻断官方客户端", func(t *testing.T) {
		result := detector.DetectWithPolicy(
			newCodexDetectorTestContext("codex-tui/0.125.0", ""),
			account,
			nil,
			CodexCLIOnlyPolicy{Blacklist: []openai.AllowedClientEntry{{UAContains: []string{"codex-tui/"}}}},
			nil,
		)
		require.True(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedGlobalBlacklist, result.Reason)
	})

	t.Run("自由白名单放行匹配客户端", func(t *testing.T) {
		result := detector.DetectWithPolicy(
			newCodexDetectorTestContext("Custom Tool/1.0", "Custom Tool"),
			account,
			nil,
			CodexCLIOnlyPolicy{Whitelist: []openai.AllowedClientEntry{{
				Originator: "Custom Tool",
				UAContains: []string{"Custom Tool/"},
			}}},
			nil,
		)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedGlobalWhitelist, result.Reason)
	})

	t.Run("配置指纹后缺失 required 信号则拒绝", func(t *testing.T) {
		result := detector.DetectWithPolicy(
			newCodexDetectorTestContext("codex-tui/0.125.0", ""),
			account,
			nil,
			CodexCLIOnlyPolicy{EngineFingerprintSignals: []openai.EngineFingerprintSignal{{
				Type:     openai.FingerprintSignalHeaderExact,
				Match:    []string{"x-codex-required"},
				Required: true,
			}}},
			nil,
		)
		require.True(t, result.Enabled)
		require.False(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonEngineFingerprintMissing, result.Reason)
	})

	t.Run("白名单可显式跳过指纹策略", func(t *testing.T) {
		result := detector.DetectWithPolicy(
			newCodexDetectorTestContext("Trusted Proxy/1.0", "Trusted Proxy"),
			account,
			nil,
			CodexCLIOnlyPolicy{
				Whitelist: []openai.AllowedClientEntry{{
					Originator:            "Trusted Proxy",
					UAContains:            []string{"Trusted Proxy/"},
					SkipEngineFingerprint: true,
				}},
				EngineFingerprintSignals: []openai.EngineFingerprintSignal{{
					Type:     openai.FingerprintSignalHeaderExact,
					Match:    []string{"x-codex-required"},
					Required: true,
				}},
			},
			nil,
		)
		require.True(t, result.Enabled)
		require.True(t, result.Matched)
		require.Equal(t, CodexClientRestrictionReasonMatchedGlobalWhitelist, result.Reason)
	})
}
