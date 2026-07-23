package service

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

var ErrAuditLogNotFound = infraerrors.NotFound("AUDIT_LOG_NOT_FOUND", "audit log not found")

const (
	AuditAuthMethodJWT                = "jwt"
	AuditAuthMethodAdminAPIKey        = "admin_api_key"
	AuditRequestBodyCaptureLimit      = 256 * 1024
	auditRequestBodyMaxBytes          = 16 * 1024
	AuditActionLogin                  = "auth.login"
	AuditActionLogin2FA               = "auth.login.2fa"
	AuditActionRegister               = "auth.register"
	AuditActionTokenRefresh           = "auth.token.refresh"
	AuditActionAuditLogClear          = "admin.audit_log.clear"
	AuditActionSessionBindingMismatch = "auth.session_binding.mismatch"
	AuditActionStepUpVerify           = "auth.step_up.verify"
)

type AuditLog struct {
	ID               int64          `json:"id"`
	CreatedAt        time.Time      `json:"created_at"`
	ActorUserID      *int64         `json:"actor_user_id,omitempty"`
	ActorEmail       string         `json:"actor_email"`
	ActorRole        string         `json:"actor_role"`
	AuthMethod       string         `json:"auth_method"`
	CredentialMasked string         `json:"credential_masked"`
	Action           string         `json:"action"`
	Method           string         `json:"method"`
	Path             string         `json:"path"`
	RequestID        string         `json:"request_id"`
	ClientIP         string         `json:"client_ip"`
	UserAgent        string         `json:"user_agent"`
	RequestBody      string         `json:"request_body,omitempty"`
	StatusCode       int            `json:"status_code"`
	LatencyMs        int64          `json:"latency_ms"`
	Extra            map[string]any `json:"extra,omitempty"`
}

type AuditLogFilter struct {
	Page, PageSize                                          int
	StartTime, EndTime                                      *time.Time
	ActorUserID                                             *int64
	ActorEmail, AuthMethod, Action, Method, ClientIP, Query string
	Success                                                 *bool
}

type AuditLogList struct {
	Logs                  []*AuditLog
	Total, Page, PageSize int
}

type AuditLogRepository interface {
	BatchInsert(context.Context, []*AuditLog) (int64, error)
	Insert(context.Context, *AuditLog) error
	List(context.Context, *AuditLogFilter) (*AuditLogList, error)
	GetByID(context.Context, int64) (*AuditLog, error)
	Count(context.Context) (int64, error)
	TruncateAll(context.Context) error
	DeleteBefore(context.Context, time.Time, int) (int64, error)
}

func auditNormalizeBodyKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(key)) {
		if r != '_' && r != '-' && r != '.' && r != ' ' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var auditBodySensitiveExactKeys = func() map[string]struct{} {
	builtin := []string{
		"code", "codes", "pin", "cvv", "authorization", "cookie", "x-api-key", "key",
		"proxy_key", "custom_key", "session", "state", "nonce",
	}
	set := make(map[string]struct{}, len(builtin)+len(SensitiveCredentialKeys)+16)
	for _, key := range builtin {
		set[auditNormalizeBodyKey(key)] = struct{}{}
	}
	for _, key := range SensitiveCredentialKeys {
		set[auditNormalizeBodyKey(key)] = struct{}{}
	}
	for _, fields := range providerSensitiveConfigFields {
		for key := range fields {
			set[auditNormalizeBodyKey(key)] = struct{}{}
		}
	}
	return set
}()

var auditBodySensitiveSubstrings = []string{
	"password", "passwd", "secret", "token", "apikey", "accesskey", "privatekey",
	"otp", "credentialvalue", "sessionkey", "serviceaccount", "verifycode",
	"verificationcode", "recoverycode", "invitationcode", "promocode", "affcode",
}

func isAuditSensitiveBodyKey(key string) bool {
	normalized := auditNormalizeBodyKey(key)
	if _, ok := auditBodySensitiveExactKeys[normalized]; ok {
		return true
	}
	for _, part := range auditBodySensitiveSubstrings {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}

func RedactAuditBody(raw []byte, contentType string) string {
	if len(raw) == 0 {
		return ""
	}
	if len(raw) > AuditRequestBodyCaptureLimit {
		return "<body omitted: exceeds " + strconv.Itoa(AuditRequestBodyCaptureLimit) + " bytes>"
	}
	if !strings.Contains(strings.ToLower(contentType), "json") || !json.Valid(raw) {
		return "<non-json body omitted>"
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "<unparsable body omitted>"
	}
	encoded, err := json.Marshal(redactAuditValue(value, 0))
	if err != nil {
		return "<redacted>"
	}
	if len(encoded) > auditRequestBodyMaxBytes {
		cutoff := auditRequestBodyMaxBytes
		for cutoff > 0 && !utf8.Valid(encoded[:cutoff]) {
			cutoff--
		}
		return string(encoded[:cutoff]) + "...<truncated>"
	}
	return string(encoded)
}

func redactAuditValue(value any, depth int) any {
	if depth > 24 {
		return "<depth limit exceeded>"
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if isAuditSensitiveBodyKey(key) {
				out[key] = "***"
			} else {
				out[key] = redactAuditValue(item, depth+1)
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = redactAuditValue(v[i], depth+1)
		}
		return out
	default:
		return value
	}
}

func MaskAuditCredential(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 14 {
		return "****"
	}
	return value[:6] + "****" + value[len(value)-4:]
}

func RedactAuditQuery(query string) string {
	if strings.TrimSpace(query) == "" {
		return ""
	}
	return logredact.RedactText(query, "api_key", "apikey", "token", "secret", "key")
}

func parseAuditLogRetentionDays(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 180
	}
	days, err := strconv.Atoi(value)
	if err != nil {
		return 180
	}
	return days
}
