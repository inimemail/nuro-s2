package service

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	CyberPolicyAnchorSessionID      = "session_id"
	CyberPolicyAnchorConversationID = "conversation_id"
	CyberPolicyAnchorPromptCacheKey = "prompt_cache_key"

	defaultCyberPolicySessionBlockTTL = 15 * time.Minute
	opsCyberPolicyKey                 = "ops_cyber_policy"
)

type CyberPolicyDecision struct {
	Matched        bool
	Message        string
	Code           string
	ErrorType      string
	AnchorType     string
	AnchorHash     string
	SessionBlocked bool
}

type cyberPolicySessionBlock struct {
	ExpiresAt time.Time
	Decision  CyberPolicyDecision
}

type CyberPolicyBlockedError struct {
	Decision CyberPolicyDecision
}

func (e *CyberPolicyBlockedError) Error() string {
	msg := strings.TrimSpace(e.Decision.Message)
	if msg == "" {
		msg = "upstream cyber_policy blocked this session"
	}
	return msg
}

// CyberPolicyMark records one upstream cyber_policy hard block on gin.Context.
// Handler-side usage recording reads this mark to label the usage row request_type=cyber.
type CyberPolicyMark struct {
	Code           string
	Message        string
	Body           string
	UpstreamStatus int
	UpstreamInTok  int
	UpstreamOutTok int
}

func MarkOpsCyberPolicy(c *gin.Context, mark CyberPolicyMark) {
	if c == nil {
		return
	}
	if GetOpsCyberPolicy(c) != nil {
		return
	}
	mark.Code = "cyber_policy"
	mark.Message = strings.TrimSpace(mark.Message)
	mark.Body = strings.TrimSpace(mark.Body)
	c.Set(opsCyberPolicyKey, &mark)
}

func GetOpsCyberPolicy(c *gin.Context) *CyberPolicyMark {
	if c == nil {
		return nil
	}
	if v, ok := c.Get(opsCyberPolicyKey); ok {
		if mark, ok := v.(*CyberPolicyMark); ok && mark != nil {
			return mark
		}
	}
	return nil
}

func ClearOpsCyberPolicy(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(opsCyberPolicyKey, (*CyberPolicyMark)(nil))
}

func DetectOpenAICyberPolicy(payload []byte) CyberPolicyDecision {
	if len(payload) == 0 {
		return CyberPolicyDecision{}
	}
	code := firstNonEmptyString(
		gjson.GetBytes(payload, "error.code").String(),
		gjson.GetBytes(payload, "response.error.code").String(),
	)
	errType := firstNonEmptyString(
		gjson.GetBytes(payload, "error.type").String(),
		gjson.GetBytes(payload, "response.error.type").String(),
	)
	message := firstNonEmptyString(
		gjson.GetBytes(payload, "error.message").String(),
		gjson.GetBytes(payload, "response.error.message").String(),
		gjson.GetBytes(payload, "message").String(),
		extractUpstreamErrorMessage(payload),
	)
	code = strings.ToLower(strings.TrimSpace(code))
	errType = strings.ToLower(strings.TrimSpace(errType))
	message = sanitizeUpstreamErrorMessage(strings.TrimSpace(message))

	if !isOpenAICyberPolicySignal(code, errType, message) {
		return CyberPolicyDecision{}
	}
	return CyberPolicyDecision{
		Matched:   true,
		Message:   message,
		Code:      code,
		ErrorType: errType,
	}
}

func isOpenAICyberPolicySignal(code, errType, message string) bool {
	if strings.EqualFold(strings.TrimSpace(code), "cyber_policy") {
		return true
	}
	combined := strings.ToLower(strings.TrimSpace(code + " " + errType + " " + message))
	if combined == "" {
		return false
	}
	if strings.Contains(combined, "high-risk cyber activity") {
		return true
	}
	if strings.Contains(combined, "high risk cyber activity") {
		return true
	}
	return strings.Contains(combined, "cyber_policy")
}

func OpenAICyberPolicyAnchor(c *gin.Context, body []byte) (anchorType, anchorHash string) {
	if c != nil {
		if raw := strings.TrimSpace(c.GetHeader("session_id")); raw != "" {
			return CyberPolicyAnchorSessionID, hashSensitiveValueForLog(raw)
		}
		if raw := strings.TrimSpace(c.GetHeader("conversation_id")); raw != "" {
			return CyberPolicyAnchorConversationID, hashSensitiveValueForLog(raw)
		}
	}
	if len(body) > 0 {
		if raw := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); raw != "" {
			return CyberPolicyAnchorPromptCacheKey, hashSensitiveValueForLog(raw)
		}
	}
	return "", ""
}

func (s *OpenAIGatewayService) openAICyberPolicyBlockKey(platform string, accountID int64, anchorType, anchorHash string) string {
	platform = strings.TrimSpace(platform)
	anchorType = strings.TrimSpace(anchorType)
	anchorHash = strings.TrimSpace(anchorHash)
	if platform == "" || accountID <= 0 || anchorType == "" || anchorHash == "" {
		return ""
	}
	return strings.Join([]string{platform, strconv.FormatInt(accountID, 10), anchorType, anchorHash}, ":")
}

func (s *OpenAIGatewayService) markOpenAICyberPolicySessionBlocked(ctx context.Context, account *Account, decision CyberPolicyDecision) CyberPolicyDecision {
	if s == nil || account == nil || !decision.Matched || decision.AnchorType == "" || decision.AnchorHash == "" {
		return decision
	}
	if !s.openAICyberPolicySessionBlockEnabled(ctx) {
		return decision
	}
	key := s.openAICyberPolicyBlockKey(account.Platform, account.ID, decision.AnchorType, decision.AnchorHash)
	if key == "" {
		return decision
	}
	block := cyberPolicySessionBlock{
		ExpiresAt: time.Now().Add(defaultCyberPolicySessionBlockTTL),
		Decision:  decision,
	}
	s.openaiCyberPolicySessionBlocks.Store(key, block)
	decision.SessionBlocked = true
	return decision
}

func (s *OpenAIGatewayService) checkOpenAICyberPolicySessionBlock(ctx context.Context, account *Account, anchorType, anchorHash string) (CyberPolicyDecision, bool) {
	if s == nil || account == nil {
		return CyberPolicyDecision{}, false
	}
	key := s.openAICyberPolicyBlockKey(account.Platform, account.ID, anchorType, anchorHash)
	if key == "" {
		return CyberPolicyDecision{}, false
	}
	value, ok := s.openaiCyberPolicySessionBlocks.Load(key)
	if !ok {
		return CyberPolicyDecision{}, false
	}
	block, ok := value.(cyberPolicySessionBlock)
	if !ok || time.Now().After(block.ExpiresAt) {
		s.openaiCyberPolicySessionBlocks.Delete(key)
		return CyberPolicyDecision{}, false
	}
	if !s.openAICyberPolicySessionBlockEnabled(ctx) {
		return CyberPolicyDecision{}, false
	}
	decision := block.Decision
	decision.Matched = true
	decision.SessionBlocked = true
	decision.AnchorType = strings.TrimSpace(anchorType)
	decision.AnchorHash = strings.TrimSpace(anchorHash)
	if decision.Message == "" {
		decision.Message = "upstream cyber_policy blocked this session"
	}
	return decision, true
}

func (s *OpenAIGatewayService) openAICyberPolicySessionBlockEnabled(ctx context.Context) bool {
	if s == nil || s.settingService == nil {
		return false
	}
	return s.settingService.IsRiskControlEnabled(ctx)
}

func requestContextFromGin(c *gin.Context) context.Context {
	if c != nil && c.Request != nil {
		return c.Request.Context()
	}
	return context.Background()
}

func (s *OpenAIGatewayService) handleOpenAICyberPolicyEvent(c *gin.Context, account *Account, passthrough bool, upstreamRequestID string, payload []byte, requestBody []byte) CyberPolicyDecision {
	decision := DetectOpenAICyberPolicy(payload)
	if !decision.Matched {
		return decision
	}
	if decision.AnchorType == "" || decision.AnchorHash == "" {
		decision.AnchorType, decision.AnchorHash = OpenAICyberPolicyAnchor(c, requestBody)
	}
	if !IsOpenAIResponsesHealthProbe(c) && decision.AnchorType != "" && decision.AnchorHash != "" {
		var ctx context.Context
		if c != nil && c.Request != nil {
			ctx = c.Request.Context()
		}
		decision = s.markOpenAICyberPolicySessionBlocked(ctx, account, decision)
	}
	markOpenAICyberPolicyOps(c, account, passthrough, upstreamRequestID, http.StatusForbidden, payload, decision)
	return decision
}

func markOpenAICyberPolicyOps(c *gin.Context, account *Account, passthrough bool, upstreamRequestID string, statusCode int, payload []byte, decision CyberPolicyDecision) {
	if c == nil || !decision.Matched {
		return
	}
	msg := strings.TrimSpace(decision.Message)
	if msg == "" {
		msg = "upstream cyber_policy blocked this request"
	}
	setOpsUpstreamError(c, statusCode, msg, "")
	event := OpsUpstreamErrorEvent{
		Platform:                  PlatformOpenAI,
		UpstreamStatusCode:        statusCode,
		UpstreamRequestID:         strings.TrimSpace(upstreamRequestID),
		Passthrough:               passthrough,
		Kind:                      "cyber_policy",
		Message:                   msg,
		CyberPolicy:               true,
		CyberPolicySessionBlocked: decision.SessionBlocked,
		CyberPolicyAnchorType:     decision.AnchorType,
		CyberPolicyAnchorHash:     decision.AnchorHash,
	}
	if account != nil {
		event.Platform = account.Platform
		event.AccountID = account.ID
		event.AccountName = account.Name
	}
	if len(payload) > 0 {
		event.UpstreamResponseBody = truncateString(string(payload), 2048)
	}
	appendOpsUpstreamError(c, event)

	usage, _ := extractOpenAIUsageFromJSONBytes(payload)
	MarkOpsCyberPolicy(c, CyberPolicyMark{
		Message:        msg,
		Body:           truncateString(string(payload), 2048),
		UpstreamStatus: statusCode,
		UpstreamInTok:  usage.InputTokens,
		UpstreamOutTok: usage.OutputTokens,
	})
}
