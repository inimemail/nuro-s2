package service

import (
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
)

const CodexOfficialClientsOnlyMessage = "This account only allows Codex official clients"

const (
	// CodexClientRestrictionReasonDisabled 表示账号未开启 codex_cli_only。
	CodexClientRestrictionReasonDisabled = "codex_cli_only_disabled"
	// CodexClientRestrictionReasonMatchedUA 表示请求命中官方客户端 UA 白名单。
	CodexClientRestrictionReasonMatchedUA = "official_client_user_agent_matched"
	// CodexClientRestrictionReasonMatchedOriginator 表示请求命中官方客户端 originator 白名单。
	CodexClientRestrictionReasonMatchedOriginator = "official_client_originator_matched"
	// CodexClientRestrictionReasonMatchedAllowedClient 表示请求命中账号级额外放行的命名客户端预设。
	CodexClientRestrictionReasonMatchedAllowedClient = "allowed_client_matched"
	// CodexClientRestrictionReasonMatchedGlobalAllowedClient 表示请求命中全局额外放行的命名客户端预设。
	CodexClientRestrictionReasonMatchedGlobalAllowedClient = "global_allowed_client_matched"
	CodexClientRestrictionReasonMatchedGlobalWhitelist     = "global_whitelist_matched"
	CodexClientRestrictionReasonMatchedGlobalBlacklist     = "global_blacklist_matched"
	CodexClientRestrictionReasonEngineFingerprintMissing   = "engine_fingerprint_missing"
	CodexClientRestrictionReasonVersionUndetectable        = "codex_version_undetectable"
	CodexClientRestrictionReasonVersionTooLow              = "codex_version_too_low"
	CodexClientRestrictionReasonVersionTooHigh             = "codex_version_too_high"
	// CodexClientRestrictionReasonNotMatchedUA 表示请求未命中官方客户端 UA 白名单。
	CodexClientRestrictionReasonNotMatchedUA = "official_client_user_agent_not_matched"
	// CodexClientRestrictionReasonForceCodexCLI 表示通过 ForceCodexCLI 配置兜底放行。
	CodexClientRestrictionReasonForceCodexCLI = "force_codex_cli_enabled"
)

type CodexCLIOnlyPolicy struct {
	Blacklist                []openai.AllowedClientEntry
	Whitelist                []openai.AllowedClientEntry
	MinCodexVersion          string
	MaxCodexVersion          string
	AllowAppServerClients    bool
	EngineFingerprintSignals []openai.EngineFingerprintSignal
}

// CodexClientRestrictionDetectionResult 是 codex_cli_only 统一检测入口结果。
type CodexClientRestrictionDetectionResult struct {
	Enabled         bool
	Matched         bool
	Reason          string
	DetectedVersion string
	MinCodexVersion string
	MaxCodexVersion string
}

// CodexClientRestrictionDetector 定义 codex_cli_only 统一检测入口。
type CodexClientRestrictionDetector interface {
	Detect(c *gin.Context, account *Account, globalAllowedClients []string) CodexClientRestrictionDetectionResult
	DetectWithPolicy(c *gin.Context, account *Account, globalAllowedClients []string, policy CodexCLIOnlyPolicy, body []byte) CodexClientRestrictionDetectionResult
}

// OpenAICodexClientRestrictionDetector 为 OpenAI OAuth codex_cli_only 的默认实现。
type OpenAICodexClientRestrictionDetector struct {
	cfg *config.Config
}

func NewOpenAICodexClientRestrictionDetector(cfg *config.Config) *OpenAICodexClientRestrictionDetector {
	return &OpenAICodexClientRestrictionDetector{cfg: cfg}
}

func (d *OpenAICodexClientRestrictionDetector) Detect(c *gin.Context, account *Account, globalAllowedClients []string) CodexClientRestrictionDetectionResult {
	return d.DetectWithPolicy(c, account, globalAllowedClients, CodexCLIOnlyPolicy{}, nil)
}

func (d *OpenAICodexClientRestrictionDetector) DetectWithPolicy(c *gin.Context, account *Account, globalAllowedClients []string, policy CodexCLIOnlyPolicy, body []byte) CodexClientRestrictionDetectionResult {
	if account == nil || !account.IsCodexCLIOnlyEnabled() {
		return CodexClientRestrictionDetectionResult{
			Enabled: false,
			Matched: false,
			Reason:  CodexClientRestrictionReasonDisabled,
		}
	}

	if d != nil && d.cfg != nil && d.cfg.Gateway.ForceCodexCLI {
		return CodexClientRestrictionDetectionResult{
			Enabled: true,
			Matched: true,
			Reason:  CodexClientRestrictionReasonForceCodexCLI,
		}
	}

	userAgent := ""
	originator := ""
	if c != nil {
		userAgent = c.GetHeader("User-Agent")
		originator = c.GetHeader("originator")
	}
	if openai.MatchDenyEntries(userAgent, originator, policy.Blacklist) {
		return CodexClientRestrictionDetectionResult{
			Enabled: true,
			Matched: false,
			Reason:  CodexClientRestrictionReasonMatchedGlobalBlacklist,
		}
	}
	if openai.IsCodexOfficialClientRequestStrict(userAgent) {
		result := CodexClientRestrictionDetectionResult{
			Enabled: true,
			Matched: true,
			Reason:  CodexClientRestrictionReasonMatchedUA,
		}
		if restricted := applyCodexVersionPolicy(userAgent, policy, result); restricted.Reason != "" {
			return restricted
		}
		return applyCodexEngineFingerprintPolicy(c, body, policy, result, false)
	}
	if openai.IsCodexOfficialClientOriginator(originator) {
		result := CodexClientRestrictionDetectionResult{
			Enabled: true,
			Matched: true,
			Reason:  CodexClientRestrictionReasonMatchedOriginator,
		}
		if restricted := applyCodexVersionPolicy(userAgent, policy, result); restricted.Reason != "" {
			return restricted
		}
		return applyCodexEngineFingerprintPolicy(c, body, policy, result, false)
	}

	// 官方客户端白名单未命中时，先尝试账号级额外放行的命名客户端预设（如 Claude Code codex 插件）。
	if allowed := account.GetCodexCLIOnlyAllowedClients(); len(allowed) > 0 &&
		openai.MatchAllowedClients(userAgent, originator, allowed) {
		result := CodexClientRestrictionDetectionResult{
			Enabled: true,
			Matched: true,
			Reason:  CodexClientRestrictionReasonMatchedAllowedClient,
		}
		return applyCodexEngineFingerprintPolicy(c, body, policy, result, false)
	}

	// 再尝试由更高作用域（全局设置）注入的额外放行客户端列表。
	if len(globalAllowedClients) > 0 &&
		openai.MatchAllowedClients(userAgent, originator, globalAllowedClients) {
		result := CodexClientRestrictionDetectionResult{
			Enabled: true,
			Matched: true,
			Reason:  CodexClientRestrictionReasonMatchedGlobalAllowedClient,
		}
		return applyCodexEngineFingerprintPolicy(c, body, policy, result, false)
	}

	if entry, ok := openai.MatchClientEntry(userAgent, originator, policy.Whitelist); ok {
		result := CodexClientRestrictionDetectionResult{
			Enabled: true,
			Matched: true,
			Reason:  CodexClientRestrictionReasonMatchedGlobalWhitelist,
		}
		return applyCodexEngineFingerprintPolicy(c, body, policy, result, entry.SkipEngineFingerprint)
	}

	return CodexClientRestrictionDetectionResult{
		Enabled: true,
		Matched: false,
		Reason:  CodexClientRestrictionReasonNotMatchedUA,
	}
}

func applyCodexVersionPolicy(userAgent string, policy CodexCLIOnlyPolicy, result CodexClientRestrictionDetectionResult) CodexClientRestrictionDetectionResult {
	if policy.MinCodexVersion == "" && policy.MaxCodexVersion == "" {
		return CodexClientRestrictionDetectionResult{}
	}
	version, ok := openai.ParseCodexEngineVersion(userAgent)
	if !ok {
		return CodexClientRestrictionDetectionResult{
			Enabled: true,
			Matched: false,
			Reason:  CodexClientRestrictionReasonVersionUndetectable,
		}
	}
	if policy.MinCodexVersion != "" && CompareVersions(version, policy.MinCodexVersion) < 0 {
		return CodexClientRestrictionDetectionResult{
			Enabled:         true,
			Matched:         false,
			Reason:          CodexClientRestrictionReasonVersionTooLow,
			DetectedVersion: version,
			MinCodexVersion: policy.MinCodexVersion,
		}
	}
	if policy.MaxCodexVersion != "" && CompareVersions(version, policy.MaxCodexVersion) > 0 {
		return CodexClientRestrictionDetectionResult{
			Enabled:         true,
			Matched:         false,
			Reason:          CodexClientRestrictionReasonVersionTooHigh,
			DetectedVersion: version,
			MaxCodexVersion: policy.MaxCodexVersion,
		}
	}
	return CodexClientRestrictionDetectionResult{}
}

func CodexClientRestrictionMessage(r CodexClientRestrictionDetectionResult) string {
	switch r.Reason {
	case CodexClientRestrictionReasonVersionTooLow:
		return fmt.Sprintf(
			"Your Codex version (%s) is below the minimum required version (%s). Please update Codex.",
			r.DetectedVersion, r.MinCodexVersion)
	case CodexClientRestrictionReasonVersionTooHigh:
		return fmt.Sprintf(
			"Your Codex version (%s) exceeds the maximum allowed version (%s). Please downgrade Codex to %s or lower.",
			r.DetectedVersion, r.MaxCodexVersion, r.MaxCodexVersion)
	default:
		return CodexOfficialClientsOnlyMessage
	}
}

func applyCodexEngineFingerprintPolicy(c *gin.Context, body []byte, policy CodexCLIOnlyPolicy, result CodexClientRestrictionDetectionResult, skip bool) CodexClientRestrictionDetectionResult {
	if !result.Matched || skip || len(policy.EngineFingerprintSignals) == 0 {
		return result
	}
	var header http.Header
	if c != nil && c.Request != nil {
		header = c.Request.Header
	}
	if openai.EvaluateEngineFingerprint(header, body, policy.EngineFingerprintSignals) {
		return result
	}
	return CodexClientRestrictionDetectionResult{
		Enabled: true,
		Matched: false,
		Reason:  CodexClientRestrictionReasonEngineFingerprintMissing,
	}
}
