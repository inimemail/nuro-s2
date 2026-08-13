package service

import (
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
)

// codexUpstreamMinVersion 上游 /backend-api/codex 接受的最低 version 头。
const codexUpstreamMinVersion = "0.144.0"

const codexClientVersionMaxLen = 64
const codexBrowserUserAgentMaxLen = 512

var codexClientVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.]+)?$`)

type codexIdentityRuntimeSnapshot struct {
	version            string
	canonicalUserAgent string
	browserUserAgent   string
	enforceIdentity    bool
}

var codexIdentityRuntime atomic.Pointer[codexIdentityRuntimeSnapshot]

func init() {
	publishCodexIdentityRuntime("", "", "", false)
}

// NormalizeCodexClientVersion accepts only the short ASCII version forms used
// by official Codex releases. The value is later emitted in HTTP headers.
func NormalizeCodexClientVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || len(version) > codexClientVersionMaxLen || !codexClientVersionPattern.MatchString(version) {
		return ""
	}
	return version
}

func resolveCodexClientVersion(manualVersion, syncedVersion string) string {
	if version := NormalizeCodexClientVersion(manualVersion); version != "" {
		return version
	}
	if version := NormalizeCodexClientVersion(syncedVersion); version != "" {
		return version
	}
	return codexCLIVersion
}

func normalizeCodexBrowserUserAgent(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" || len(userAgent) > codexBrowserUserAgentMaxLen {
		return DefaultOpenAICodexUserAgent
	}
	for i := 0; i < len(userAgent); i++ {
		if userAgent[i] < 0x20 || userAgent[i] == 0x7f {
			return DefaultOpenAICodexUserAgent
		}
	}
	return userAgent
}

// publishCodexIdentityRuntime is called only by startup/background/settings
// paths. Gateway requests consume the immutable snapshot with one atomic load.
func publishCodexIdentityRuntime(manualVersion, syncedVersion, browserUserAgent string, enforce bool) {
	version := resolveCodexClientVersion(manualVersion, syncedVersion)
	explicitVersion := NormalizeCodexClientVersion(manualVersion)
	if explicitVersion == "" {
		explicitVersion = NormalizeCodexClientVersion(syncedVersion)
	}
	canonicalUA := openai.SetCodexUserAgentVersion(codexCLIUserAgent, version)
	if canonicalUA == "" {
		canonicalUA = codexCLIUserAgent
		version = codexCLIVersion
	}
	browserUA := normalizeCodexBrowserUserAgent(browserUserAgent)
	if explicitVersion != "" {
		if rebuilt := openai.SetCodexUserAgentVersion(browserUA, version); rebuilt != "" {
			browserUA = rebuilt
		}
	}
	codexIdentityRuntime.Store(&codexIdentityRuntimeSnapshot{
		version:            version,
		canonicalUserAgent: canonicalUA,
		browserUserAgent:   browserUA,
		enforceIdentity:    enforce,
	})
}

func currentCodexIdentityRuntime() *codexIdentityRuntimeSnapshot {
	if snapshot := codexIdentityRuntime.Load(); snapshot != nil {
		return snapshot
	}
	return &codexIdentityRuntimeSnapshot{
		version:            codexCLIVersion,
		canonicalUserAgent: codexCLIUserAgent,
		browserUserAgent:   DefaultOpenAICodexUserAgent,
	}
}

// ensureCodexIdentityHeaders fills the identity headers required by the
// ChatGPT Codex upstream. Existing user-agent and version values are retained
// for enforceCodexIdentityHeaders to normalize afterward.
func ensureCodexIdentityHeaders(h http.Header) {
	if h == nil {
		return
	}
	snapshot := currentCodexIdentityRuntime()
	if strings.TrimSpace(h.Get("user-agent")) == "" {
		h.Set("user-agent", snapshot.canonicalUserAgent)
	}
	if strings.TrimSpace(h.Get("originator")) == "" {
		h.Set("originator", "codex_cli_rs")
	}
	if strings.TrimSpace(h.Get("version")) == "" {
		h.Set("version", snapshot.version)
	}
	h.Set("OpenAI-Beta", "responses=experimental")
}

// applyOpenAICodexProbeHeaders gives synthetic Responses probes the same
// protocol identity as the real Codex path without involving scheduler state.
func applyOpenAICodexProbeHeaders(h http.Header) {
	if h == nil {
		return
	}
	ensureCodexIdentityHeaders(h)
	h.Set("X-Codex-Window-ID", uuid.NewString())
}

// enforceCodexIdentityHeaders 收口 OAuth（ChatGPT 内部接口）出站请求的客户端身份头。
// 仅对携带 originator 的请求生效；需要从缺失身份头恢复的调用方应先调用
// ensureCodexIdentityHeaders。必须在所有 User-Agent 改写之后调用。
func enforceCodexIdentityHeaders(h http.Header) {
	if h == nil || h.Get("originator") == "" {
		return
	}
	snapshot := currentCodexIdentityRuntime()
	if snapshot.enforceIdentity {
		h.Set("user-agent", snapshot.canonicalUserAgent)
		h.Set("originator", "codex_cli_rs")
		h.Set("version", snapshot.version)
		return
	}
	originator, pairedUA, ok := openai.PairCodexClientIdentity(h.Get("user-agent"))
	if !ok {
		originator, pairedUA = "codex_cli_rs", snapshot.canonicalUserAgent
	}
	h.Set("user-agent", pairedUA)
	h.Set("originator", originator)
	if v := strings.TrimSpace(h.Get("version")); v != "" && CompareVersions(v, codexUpstreamMinVersion) < 0 {
		h.Set("version", snapshot.version)
	}
}
