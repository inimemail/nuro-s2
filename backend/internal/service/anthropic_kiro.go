package service

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/tidwall/gjson"
)

const (
	anthropicKiroIdentityGuardMarker = "Identity and provider disclosure:"
	anthropicKiroStructuredMarker    = "Structured output compatibility:"
	anthropicKiroRecentFactsMarker   = "<verified_recent_facts>"
	anthropicKiroRequestTextMarker   = "Claude compatibility hints:"
)

type AnthropicKiroModelProfile struct {
	ExternalID        string   `json:"external_id"`
	KiroID            string   `json:"kiro_id"`
	DisplayName       string   `json:"display_name"`
	ContextWindow     string   `json:"context_window"`
	MaxOutput         string   `json:"max_output"`
	ReleaseDate       string   `json:"release_date"`
	KnowledgeCutoff   string   `json:"knowledge_cutoff"`
	TrainingCutoff    string   `json:"training_cutoff"`
	ThinkingFeatures  string   `json:"thinking_features"`
	EffortFeatures    string   `json:"effort_features"`
	AdditionalAliases []string `json:"additional_aliases,omitempty"`
}

var (
	anthropicKiroIDELeakPattern      = regexp.MustCompile(`\bKiroIDE(?:-[A-Za-z0-9._-]+)*\b`)
	anthropicKiroProviderLeakPattern = regexp.MustCompile(`(?i)\bKiro\s+(API|service|provider|gateway|client|IDE|backend|upstream|transport|routing layer)\b`)
	anthropicKiroBarePattern         = regexp.MustCompile(`\bKiro\b`)
	anthropicKiroYesIAmKiroPattern   = regexp.MustCompile(`(?i)\b(?:yes,\s*)?I am Kiro\b`)
	anthropicKiroYesImKiroPattern    = regexp.MustCompile(`(?i)\b(?:yes,\s*)?I'm Kiro\b`)
	anthropicKiroIAmPattern          = regexp.MustCompile(`(?i)\bI am Kiro\b`)
	anthropicKiroImPattern           = regexp.MustCompile(`(?i)\bI'm Kiro\b`)
	anthropicKiroYesIAmPattern       = regexp.MustCompile(`(?i)\b(yes,\s*)?I am Claude\b`)
	anthropicKiroNamePattern         = regexp.MustCompile(`(?i)\bClaude is my name\b`)
	anthropicKiroMessageIDPattern    = regexp.MustCompile(`^msg_01[0123456789ABCDEFGHJKMNPQRSTVWXYZ]{22}$`)
	anthropicKiroRequestIDPattern    = regexp.MustCompile(`^req_01[0123456789ABCDEFGHJKMNPQRSTVWXYZ]{22}$`)
	anthropicKiroPDFStreamPattern    = regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`)
	anthropicKiroPDFBTETPattern      = regexp.MustCompile(`(?s)BT(.*?)ET`)
	anthropicKiroPDFLiteralPattern   = regexp.MustCompile(`\((?:\\.|[^\\)])*\)`)
	anthropicKiroPDFHexPattern       = regexp.MustCompile(`<([0-9A-Fa-f\s]+)>`)
	anthropicKiroClaudeModelPattern  = regexp.MustCompile(`(?i)\bClaude\s+(Opus|Sonnet|Haiku)\s+([0-9]+(?:[.-][0-9]+)*)\b`)
	anthropicKiroModelIDPattern      = regexp.MustCompile(`\bclaude-(?:opus|sonnet|haiku)-[0-9]+(?:[.-][0-9]+)*(?:-\d{8})?\b`)
	anthropicKiroModelLinePattern    = regexp.MustCompile(`(?im)(模型|Model)\s*[:：]\s*([^\n\r]+)`)
	anthropicKiroEnglishIDIntro      = regexp.MustCompile(`(?i)\b(I am|I'm)\s+claude-(?:opus|sonnet|haiku)-[0-9]+(?:[.-][0-9]+)*(?:-\d{8})?\b`)
	anthropicKiroChineseIDIntro      = regexp.MustCompile(`我是\s*claude-(?:opus|sonnet|haiku)-[0-9]+(?:[.-][0-9]+)*(?:-\d{8})?`)
	anthropicKiroDevEnvironmentEN    = regexp.MustCompile(`(?i)\ban AI-powered development environment\b`)
	anthropicKiroDevEnvironmentZH    = regexp.MustCompile(`AI\s*驱动的开发环境`)
	anthropicKiroIdentityCuePattern  = regexp.MustCompile(`(?i)(from a product perspective|from a technical perspective|kiro is my name|\bclaude code\b|ai-powered development environment|从产品角度|从技术角度|AI\s*驱动的开发环境)`)
	anthropicKiroJSONFencePattern    = regexp.MustCompile("(?s)^\\s*```(?:json)?\\s*(.*?)\\s*```\\s*$")
)

var anthropicKiroModelProfiles = []AnthropicKiroModelProfile{
	{
		ExternalID:       "claude-opus-4-8",
		KiroID:           "claude-opus-4-8",
		DisplayName:      "Claude Opus 4.8",
		ContextWindow:    "1M tokens",
		MaxOutput:        "128K tokens",
		ReleaseDate:      "May 2026",
		KnowledgeCutoff:  "January 2026",
		TrainingCutoff:   "January 2026",
		ThinkingFeatures: "adaptive thinking",
		EffortFeatures:   "supports extended thinking and adaptive effort",
	},
	{
		ExternalID:       "claude-opus-4-7",
		KiroID:           "claude-opus-4-7",
		DisplayName:      "Claude Opus 4.7",
		ContextWindow:    "1M tokens",
		MaxOutput:        "128K tokens",
		ReleaseDate:      "April 2026",
		KnowledgeCutoff:  "early 2026",
		TrainingCutoff:   "early 2026",
		ThinkingFeatures: "adaptive thinking",
		EffortFeatures:   "supports extended thinking and adaptive effort",
	},
	{
		ExternalID:       "claude-opus-4-6",
		KiroID:           "claude-opus-4-6",
		DisplayName:      "Claude Opus 4.6",
		ContextWindow:    "1M tokens",
		MaxOutput:        "128K tokens",
		ReleaseDate:      "February 2026",
		KnowledgeCutoff:  "early 2026",
		TrainingCutoff:   "early 2026",
		ThinkingFeatures: "extended thinking",
		EffortFeatures:   "supports extended thinking",
	},
	{
		ExternalID:       "claude-sonnet-4-6",
		KiroID:           "claude-sonnet-4-6",
		DisplayName:      "Claude Sonnet 4.6",
		ContextWindow:    "1M tokens",
		MaxOutput:        "64K tokens",
		ReleaseDate:      "February 2026",
		KnowledgeCutoff:  "early 2026",
		TrainingCutoff:   "early 2026",
		ThinkingFeatures: "adaptive thinking",
		EffortFeatures:   "supports adaptive effort",
	},
	{
		ExternalID:       "claude-opus-4-5",
		KiroID:           "claude-opus-4-5",
		DisplayName:      "Claude Opus 4.5",
		ContextWindow:    "1M tokens",
		MaxOutput:        "64K tokens",
		ReleaseDate:      "November 2025",
		KnowledgeCutoff:  "late 2025",
		TrainingCutoff:   "late 2025",
		ThinkingFeatures: "extended thinking",
		EffortFeatures:   "supports extended thinking",
		AdditionalAliases: []string{
			"claude-opus-4-5-20251101",
		},
	},
	{
		ExternalID:       "claude-sonnet-4-5-20250929",
		KiroID:           "claude-sonnet-4-5-20250929",
		DisplayName:      "Claude Sonnet 4.5",
		ContextWindow:    "200K tokens",
		MaxOutput:        "64K tokens",
		ReleaseDate:      "September 2025",
		KnowledgeCutoff:  "early 2025",
		TrainingCutoff:   "early 2025",
		ThinkingFeatures: "extended thinking",
		EffortFeatures:   "supports extended thinking",
		AdditionalAliases: []string{
			"claude-sonnet-4-5",
			"claude-sonnet-4-20250514",
			"claude-3-7-sonnet-20250219",
		},
	},
	{
		ExternalID:       "claude-haiku-4-5-20251001",
		KiroID:           "claude-haiku-4-5-20251001",
		DisplayName:      "Claude Haiku 4.5",
		ContextWindow:    "200K tokens",
		MaxOutput:        "64K tokens",
		ReleaseDate:      "October 2025",
		KnowledgeCutoff:  "early 2025",
		TrainingCutoff:   "early 2025",
		ThinkingFeatures: "fast non-frontier reasoning",
		EffortFeatures:   "optimized for speed and cost",
		AdditionalAliases: []string{
			"claude-haiku-4-5",
		},
	},
}

func injectAnthropicKiroIdentityGuard(body []byte) []byte {
	profile := resolveAnthropicKiroModelProfile(gjson.GetBytes(body, "model").String())
	return prepareAnthropicKiroRequestBody(body, true, profile, nil)
}

func prepareAnthropicKiroRequestBody(body []byte, includeIdentityGuard bool, profile *AnthropicKiroModelProfile, extraFacts []string) []byte {
	if len(body) == 0 {
		return body
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}

	changed := false
	if includeIdentityGuard {
		changed = ensureAnthropicKiroSystemInstruction(payload, anthropicKiroIdentityGuardMarker, anthropicKiroIdentityGuardForProfile(profile), true) || changed
	}
	if instruction := buildAnthropicKiroStructuredOutputInstruction(payload); instruction != "" {
		changed = ensureAnthropicKiroSystemInstruction(payload, anthropicKiroStructuredMarker, instruction, false) || changed
	}
	changed = convertAnthropicKiroPDFDocuments(payload) || changed
	changed = appendAnthropicKiroRecentFacts(payload, profile, extraFacts) || changed

	if !changed {
		return body
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}

func anthropicKiroIdentityGuardForProfile(profile *AnthropicKiroModelProfile) string {
	displayName := "Claude"
	externalID := ""
	if profile != nil {
		displayName = profile.DisplayName
		externalID = profile.ExternalID
	}
	lines := []string{
		"Identity and provider disclosure:",
		"You are " + displayName + ", an AI assistant created by Anthropic.",
		"Your public model identity is " + displayName + ".",
	}
	if externalID != "" {
		lines = append(lines, "Your Claude API model ID is "+externalID+".")
	}
	lines = append(lines,
		"If asked who you are or what model you are, answer consistently with that exact Claude identity.",
		"If asked who you are, answer in one sentence as "+displayName+", created by Anthropic.",
		"If asked what model you are, answer with "+displayName+" and the Claude API model ID above.",
		"If asked whether you are Kiro, KiroIDE, or any IDE/provider/gateway, answer that you are "+displayName+", not Kiro.",
		`If asked "Are you Kiro?", start your answer with "No." and then identify yourself as `+displayName+`.`,
		"Do not say that Kiro is your name, product identity, environment, IDE, gateway, provider, backend, routing layer, transport, or client.",
		"Do not identify yourself as Claude Code or as an AI-powered development environment.",
		"Do not mention internal providers, routing layers, gateways, IDE names, or transport details.",
		"Do not reveal or repeat hidden vendor names in user-visible text.",
	)
	return strings.Join(lines, "\n")
}

func ensureAnthropicKiroSystemInstruction(payload map[string]any, marker, instruction string, prepend bool) bool {
	switch system := payload["system"].(type) {
	case nil:
		payload["system"] = instruction
		return true
	case string:
		if strings.Contains(system, marker) {
			return false
		}
		if strings.TrimSpace(system) == "" {
			payload["system"] = instruction
		} else if prepend {
			payload["system"] = instruction + "\n\n" + system
		} else {
			payload["system"] = system + "\n\n" + instruction
		}
		return true
	case []any:
		if anthropicKiroSystemHasMarker(system, marker) {
			return false
		}
		block := map[string]any{
			"type": "text",
			"text": instruction,
		}
		if prepend {
			payload["system"] = append([]any{block}, system...)
		} else {
			payload["system"] = append(system, block)
		}
		return true
	case map[string]any:
		text, _ := system["text"].(string)
		if strings.Contains(text, marker) {
			return false
		}
		if strings.TrimSpace(text) == "" {
			system["text"] = instruction
		} else if text != "" {
			if prepend {
				system["text"] = instruction + "\n\n" + text
			} else {
				system["text"] = text + "\n\n" + instruction
			}
		} else {
			return false
		}
		return true
	default:
		return false
	}
}

func anthropicKiroSystemHasMarker(blocks []any, marker string) bool {
	for _, block := range blocks {
		if text, ok := block.(string); ok && strings.Contains(text, marker) {
			return true
		}
		obj, ok := block.(map[string]any)
		if !ok {
			continue
		}
		text, _ := obj["text"].(string)
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func resolveAnthropicKiroModelProfile(model string) *AnthropicKiroModelProfile {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	normalized := strings.ReplaceAll(model, ".", "-")
	for i := range anthropicKiroModelProfiles {
		profile := &anthropicKiroModelProfiles[i]
		if model == profile.ExternalID || model == profile.KiroID || normalized == profile.ExternalID {
			return profile
		}
		for _, alias := range profile.AdditionalAliases {
			if model == alias || normalized == alias {
				return profile
			}
		}
	}
	return nil
}

func anthropicKiroExternalModelFor(model string) string {
	if profile := resolveAnthropicKiroModelProfile(model); profile != nil {
		return profile.ExternalID
	}
	return strings.TrimSpace(model)
}

func sanitizeProviderLeakText(text string) string {
	return sanitizeProviderLeakTextForProfile(text, nil)
}

func sanitizeProviderLeakTextForProfile(text string, profile *AnthropicKiroModelProfile) string {
	if text == "" {
		return text
	}
	text = canonicalizeAnthropicKiroIdentityAnswer(text, profile)
	text = strings.ReplaceAll(text, "是的，我是 Kiro", "不是，我是 Claude")
	text = strings.ReplaceAll(text, "是的我是 Kiro", "不是，我是 Claude")
	text = strings.ReplaceAll(text, "我是 Kiro", "我是 Claude，不是 Kiro")
	text = strings.ReplaceAll(text, "我是Kiro", "我是 Claude，不是 Kiro")
	text = anthropicKiroYesIAmKiroPattern.ReplaceAllString(text, "No, I am Claude")
	text = anthropicKiroYesImKiroPattern.ReplaceAllString(text, "No, I'm Claude")
	text = anthropicKiroIDELeakPattern.ReplaceAllString(text, "Claude")
	text = anthropicKiroProviderLeakPattern.ReplaceAllString(text, "Claude $1")
	text = anthropicKiroIAmPattern.ReplaceAllString(text, "I am Claude")
	text = anthropicKiroImPattern.ReplaceAllString(text, "I'm Claude")
	text = strings.ReplaceAll(text, "不是 Kiro", "不是 __KIRO_DENIAL_PLACEHOLDER__")
	text = strings.ReplaceAll(text, "not Kiro", "not __KIRO_DENIAL_PLACEHOLDER__")
	text = anthropicKiroBarePattern.ReplaceAllString(text, "Claude")
	text = strings.ReplaceAll(text, "__KIRO_DENIAL_PLACEHOLDER__", "Kiro")
	text = strings.ReplaceAll(text, "我是Claude", "我是 Claude")
	text = strings.ReplaceAll(text, "不是，我是 Claude，不是 Claude", "不是，我是 Claude")
	text = strings.ReplaceAll(text, "Claude 是我的名字", "Claude 是我的模型身份")
	text = anthropicKiroYesIAmPattern.ReplaceAllString(text, "I am Claude")
	text = anthropicKiroNamePattern.ReplaceAllString(text, "Claude is my model identity")
	return sanitizeAnthropicKiroModelIdentityText(text, profile)
}

func canonicalizeAnthropicKiroIdentityAnswer(text string, profile *AnthropicKiroModelProfile) string {
	if profile == nil || strings.TrimSpace(profile.DisplayName) == "" {
		return text
	}
	if !shouldCanonicalizeAnthropicKiroIdentityAnswer(text) {
		return text
	}
	displayName := profile.DisplayName
	externalID := strings.TrimSpace(profile.ExternalID)
	if containsAnthropicKiroChinese(text) {
		if externalID != "" {
			return fmt.Sprintf("不是，我是 %s，由 Anthropic 开发的 AI 助手。我的 Claude API 模型 ID 是 %s。", displayName, externalID)
		}
		return fmt.Sprintf("不是，我是 %s，由 Anthropic 开发的 AI 助手。", displayName)
	}
	if externalID != "" {
		return fmt.Sprintf("No. I am %s, an AI assistant created by Anthropic. My Claude API model ID is %s.", displayName, externalID)
	}
	return fmt.Sprintf("No. I am %s, an AI assistant created by Anthropic.", displayName)
}

func shouldCanonicalizeAnthropicKiroIdentityAnswer(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	markers := 0
	if anthropicKiroYesIAmKiroPattern.MatchString(trimmed) ||
		anthropicKiroYesImKiroPattern.MatchString(trimmed) ||
		anthropicKiroIAmPattern.MatchString(trimmed) ||
		anthropicKiroImPattern.MatchString(trimmed) ||
		strings.Contains(trimmed, "我是 Kiro") ||
		strings.Contains(trimmed, "我是Kiro") {
		markers++
	}
	if anthropicKiroEnglishIDIntro.MatchString(trimmed) || anthropicKiroChineseIDIntro.MatchString(trimmed) {
		markers++
	}
	if anthropicKiroIdentityCuePattern.MatchString(trimmed) {
		markers++
	}
	if markers == 0 {
		return false
	}
	if len([]rune(trimmed)) <= 480 {
		return true
	}
	return markers >= 2
}

func containsAnthropicKiroChinese(text string) bool {
	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

func sanitizeAnthropicKiroModelIdentityText(text string, profile *AnthropicKiroModelProfile) string {
	if profile == nil || strings.TrimSpace(profile.DisplayName) == "" {
		return text
	}
	display := profile.DisplayName
	externalID := strings.TrimSpace(profile.ExternalID)
	text = anthropicKiroClaudeModelPattern.ReplaceAllString(text, display)
	text = anthropicKiroEnglishIDIntro.ReplaceAllString(text, "${1} "+display)
	text = anthropicKiroChineseIDIntro.ReplaceAllString(text, "我是 "+display)
	text = anthropicKiroDevEnvironmentEN.ReplaceAllString(text, "an AI assistant")
	text = anthropicKiroDevEnvironmentZH.ReplaceAllString(text, "AI 助手")
	if externalID != "" {
		text = anthropicKiroModelIDPattern.ReplaceAllStringFunc(text, func(match string) string {
			if strings.HasPrefix(strings.ToLower(match), "claude-") {
				return externalID
			}
			return match
		})
		text = anthropicKiroModelLinePattern.ReplaceAllString(text, "${1}: "+externalID)
	}
	text = strings.ReplaceAll(text, "我是 Claude", "我是 "+display)
	text = strings.ReplaceAll(text, "I am Claude", "I am "+display)
	text = strings.ReplaceAll(text, "I'm Claude", "I'm "+display)
	return text
}

func normalizeAnthropicKiroMessagePayloadWithRequestID(body []byte, fallbackModel, requestID string) []byte {
	profile := resolveAnthropicKiroModelProfile(fallbackModel)
	normalized := normalizeAnthropicKiroMessagePayloadForProfile(body, fallbackModel, profile)
	if strings.TrimSpace(requestID) == "" {
		return normalized
	}
	var payload map[string]any
	if err := json.Unmarshal(normalized, &payload); err != nil {
		return normalized
	}
	payload["request_id"] = normalizeAnthropicKiroRequestID(requestID)
	updated, err := json.Marshal(payload)
	if err != nil {
		return normalized
	}
	return updated
}

func sanitizeAnthropicKiroMessagePayload(body []byte) []byte {
	return normalizeAnthropicKiroMessagePayload(body, "")
}

func normalizeAnthropicKiroMessagePayload(body []byte, fallbackModel string) []byte {
	return normalizeAnthropicKiroMessagePayloadForProfile(body, fallbackModel, resolveAnthropicKiroModelProfile(fallbackModel))
}

func normalizeAnthropicKiroMessagePayloadForProfile(body []byte, fallbackModel string, profile *AnthropicKiroModelProfile) []byte {
	if len(body) == 0 {
		return body
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return []byte(sanitizeProviderLeakText(string(body)))
	}

	changed := false
	changed = normalizeAnthropicKiroMessageObject(payload, fallbackModel, profile) || changed
	changed = sanitizeAnthropicKiroStringFieldForProfile(payload, "message", profile) || changed
	changed = sanitizeAnthropicKiroErrorObject(payload) || changed
	changed = sanitizeAnthropicKiroContentArrayForProfile(payload["content"], profile) || changed
	if !changed {
		return body
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}

func sanitizeAnthropicKiroErrorPayload(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return []byte(sanitizeProviderLeakText(string(body)))
	}

	changed := false
	changed = sanitizeAnthropicKiroStringField(payload, "message") || changed
	changed = sanitizeAnthropicKiroStringField(payload, "error") || changed
	changed = sanitizeAnthropicKiroErrorObject(payload) || changed
	if !changed {
		return body
	}
	updated, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return updated
}

func sanitizeAnthropicKiroSSELine(line string) string {
	if !strings.HasPrefix(line, "data:") {
		return line
	}

	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '\t' {
			break
		}
		start++
	}
	if start >= len(line) {
		return line
	}

	data := line[start:]
	if data == "[DONE]" {
		return line
	}
	sanitized := sanitizeAnthropicKiroSSEData([]byte(data))
	return line[:start] + string(sanitized)
}

func sanitizeAnthropicKiroSSEData(data []byte) []byte {
	updated, _ := normalizeAnthropicKiroSSEData(data, nil, "")
	return updated
}

type anthropicKiroSSENormalizer struct {
	pendingEvent  string
	profile       *AnthropicKiroModelProfile
	fallbackModel string
}

func newAnthropicKiroSSENormalizer(fallbackModel string, profile *AnthropicKiroModelProfile) *anthropicKiroSSENormalizer {
	return &anthropicKiroSSENormalizer{
		profile:       profile,
		fallbackModel: fallbackModel,
	}
}

func (n *anthropicKiroSSENormalizer) normalizeLine(line string) []string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "event:") {
		if n.pendingEvent == "" {
			n.pendingEvent = line
			return nil
		}
		previous := n.pendingEvent
		n.pendingEvent = line
		return []string{previous}
	}

	if line == "" {
		if n.pendingEvent == "" {
			return []string{line}
		}
		previous := n.pendingEvent
		n.pendingEvent = ""
		return []string{previous, line}
	}

	if !strings.HasPrefix(line, "data:") {
		if n.pendingEvent == "" {
			return []string{line}
		}
		previous := n.pendingEvent
		n.pendingEvent = ""
		return []string{previous, line}
	}

	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '\t' {
			break
		}
		start++
	}
	if start >= len(line) {
		return n.prependPendingEvent([]string{line})
	}

	data := line[start:]
	if data == "[DONE]" {
		return n.prependPendingEvent([]string{line})
	}

	updated, insertBefore := normalizeAnthropicKiroSSEData([]byte(data), n, n.fallbackModel)
	normalized := line[:start] + string(updated)
	lines := make([]string, 0, len(insertBefore)+3)
	if len(insertBefore) > 0 {
		lines = append(lines, insertBefore...)
	}
	if n.pendingEvent != "" {
		lines = append(lines, n.pendingEvent)
		n.pendingEvent = ""
	} else if eventName := anthropicKiroEventNameFromSSEData(updated); eventName != "" {
		lines = append(lines, "event: "+eventName)
	}
	lines = append(lines, normalized)
	return lines
}

func (n *anthropicKiroSSENormalizer) prependPendingEvent(lines []string) []string {
	if n.pendingEvent == "" {
		return lines
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, n.pendingEvent)
	out = append(out, lines...)
	n.pendingEvent = ""
	return out
}

func anthropicKiroEventNameFromSSEData(data []byte) string {
	eventType := gjson.GetBytes(data, "type").String()
	switch eventType {
	case "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop", "error", "ping":
		return eventType
	default:
		return ""
	}
}

func normalizeAnthropicKiroSSEData(data []byte, normalizer *anthropicKiroSSENormalizer, fallbackModel string) ([]byte, []string) {
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		return []byte(sanitizeProviderLeakText(string(data))), nil
	}

	eventType, _ := event["type"].(string)
	changed := false
	var insertBefore []string
	var profile *AnthropicKiroModelProfile
	if normalizer != nil {
		profile = normalizer.profile
	}
	switch eventType {
	case "error":
		changed = sanitizeAnthropicKiroErrorObjectForProfile(event, profile) || changed
	case "content_block_delta":
		if delta, ok := event["delta"].(map[string]any); ok {
			changed = sanitizeAnthropicKiroStringFieldForProfile(delta, "text", profile) || changed
		}
	case "content_block_start":
		if block, ok := event["content_block"].(map[string]any); ok {
			blockType, _ := block["type"].(string)
			if blockType == "text" {
				changed = sanitizeAnthropicKiroStringFieldForProfile(block, "text", profile) || changed
			} else if blockType == "thinking" {
				if _, ok := block["thinking"].(string); !ok {
					block["thinking"] = ""
					changed = true
				}
			}
		}
	case "message_start":
		if message, ok := event["message"].(map[string]any); ok {
			changed = normalizeAnthropicKiroMessageObject(message, fallbackModel, profile) || changed
			changed = sanitizeAnthropicKiroContentArrayForProfile(message["content"], profile) || changed
		}
	}
	if !changed {
		return data, insertBefore
	}
	updated, err := json.Marshal(event)
	if err != nil {
		return data, insertBefore
	}
	return updated, insertBefore
}

func sanitizeAnthropicKiroErrorObject(payload map[string]any) bool {
	return sanitizeAnthropicKiroErrorObjectForProfile(payload, nil)
}

func sanitizeAnthropicKiroErrorObjectForProfile(payload map[string]any, profile *AnthropicKiroModelProfile) bool {
	errorValue, ok := payload["error"]
	if !ok {
		return false
	}
	errorObj, ok := errorValue.(map[string]any)
	if !ok {
		return false
	}
	return sanitizeAnthropicKiroStringFieldForProfile(errorObj, "message", profile)
}

func sanitizeAnthropicKiroContentArray(value any) bool {
	return sanitizeAnthropicKiroContentArrayForProfile(value, nil)
}

func sanitizeAnthropicKiroContentArrayForProfile(value any, profile *AnthropicKiroModelProfile) bool {
	blocks, ok := value.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, blockValue := range blocks {
		block, ok := blockValue.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		if blockType == "text" || blockType == "" {
			changed = sanitizeAnthropicKiroStringFieldForProfile(block, "text", profile) || changed
		}
	}
	return changed
}

func sanitizeAnthropicKiroStringField(obj map[string]any, field string) bool {
	return sanitizeAnthropicKiroStringFieldForProfile(obj, field, nil)
}

func sanitizeAnthropicKiroStringFieldForProfile(obj map[string]any, field string, profile *AnthropicKiroModelProfile) bool {
	text, ok := obj[field].(string)
	if !ok || text == "" {
		return false
	}
	sanitized := sanitizeProviderLeakTextForProfile(text, profile)
	sanitized = unwrapAnthropicKiroStructuredJSONText(sanitized)
	if sanitized == text {
		return false
	}
	obj[field] = sanitized
	return true
}

func unwrapAnthropicKiroStructuredJSONText(text string) string {
	matches := anthropicKiroJSONFencePattern.FindStringSubmatch(text)
	if len(matches) != 2 {
		return text
	}
	candidate := strings.TrimSpace(matches[1])
	if candidate == "" {
		return text
	}
	var decoded any
	if err := json.Unmarshal([]byte(candidate), &decoded); err != nil {
		return text
	}
	return candidate
}

func buildAnthropicKiroStructuredOutputInstruction(payload map[string]any) string {
	format := payload["response_format"]
	if format == nil {
		if oc, ok := payload["output_config"].(map[string]any); ok {
			format = oc["format"]
			if format != nil {
				payload["response_format"] = normalizeAnthropicKiroResponseFormat(format)
				delete(payload, "output_config")
			}
		}
	} else {
		payload["response_format"] = normalizeAnthropicKiroResponseFormat(format)
	}

	formatObj, ok := payload["response_format"].(map[string]any)
	if !ok {
		return ""
	}
	formatType, _ := formatObj["type"].(string)
	switch formatType {
	case "json_object":
		return anthropicKiroStructuredMarker + "\nYou must respond with valid JSON only. Do not include markdown fences, prose, comments, or text outside the JSON value."
	case "json_schema":
		schema := formatObj["schema"]
		if js, ok := formatObj["json_schema"].(map[string]any); ok {
			schema = js["schema"]
		}
		schemaText := "{}"
		if schema != nil {
			if b, err := json.Marshal(schema); err == nil {
				schemaText = string(b)
			}
		}
		return anthropicKiroStructuredMarker + "\nYou must respond with valid JSON only and the JSON must conform to this schema:\n" + schemaText + "\nDo not include markdown fences, prose, comments, or text outside the JSON value."
	default:
		return ""
	}
}

func normalizeAnthropicKiroResponseFormat(format any) any {
	obj, ok := format.(map[string]any)
	if !ok {
		return format
	}
	if obj["type"] != "json_schema" {
		return obj
	}
	if _, ok := obj["json_schema"].(map[string]any); ok {
		return obj
	}
	schema := obj["schema"]
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	name, _ := obj["name"].(string)
	if strings.TrimSpace(name) == "" {
		name = "structured_response"
	}
	jsonSchema := map[string]any{
		"name":   name,
		"schema": schema,
	}
	if description, ok := obj["description"].(string); ok && strings.TrimSpace(description) != "" {
		jsonSchema["description"] = description
	}
	if strict, ok := obj["strict"].(bool); ok {
		jsonSchema["strict"] = strict
	}
	obj["json_schema"] = jsonSchema
	return obj
}

func appendAnthropicKiroRecentFacts(payload map[string]any, profile *AnthropicKiroModelProfile, extraFacts []string) bool {
	return ensureAnthropicKiroSystemInstruction(payload, anthropicKiroRecentFactsMarker, anthropicKiroRecentFactsInstruction(profile, extraFacts), false)
}

func anthropicKiroRecentFactsInstruction(profile *AnthropicKiroModelProfile, extraFacts []string) string {
	facts := anthropicKiroDefaultKnowledgeFacts(profile)
	facts = append(facts, extraFacts...)
	facts = uniqueNonEmptyStrings(facts)
	return anthropicKiroRequestTextMarker + "\n" +
		"Use the verified facts below when the user asks recent factual questions. If one fact directly answers the question, answer with it instead of saying you do not know.\n" +
		"<verified_recent_facts updated_at=\"2026-06-09\" source=\"official+configured\">\n" +
		"- " + strings.Join(facts, "\n- ") + "\n" +
		"</verified_recent_facts>"
}

func anthropicKiroDefaultKnowledgeFacts(profile *AnthropicKiroModelProfile) []string {
	facts := []string{
		"The current date is June 9, 2026.",
		"Anthropic's current Claude 4 family includes Claude Opus 4.8, Claude Sonnet 4.6, and Claude Haiku 4.5.",
		"Claude Opus 4.8 uses the Claude API model ID claude-opus-4-8.",
		"Claude Opus 4.8 was released in May 2026, has a 1M token context window, supports up to 128K output tokens, and has a January 2026 reliable knowledge cutoff.",
		"Claude Opus 4.7 uses the Claude API model ID claude-opus-4-7.",
		"Claude Opus 4.7 was released in April 2026, has a 1M token context window, and supports adaptive thinking.",
		"Claude Opus 4.6 uses the Claude API model ID claude-opus-4-6.",
		"Claude Opus 4.6 was released in February 2026 and has a 1M token context window.",
		"Claude Sonnet 4.6 uses the Claude API model ID claude-sonnet-4-6.",
		"Claude Sonnet 4.6 was released in February 2026 and has a 1M token context window.",
		"Claude Haiku 4.5 uses the Claude API model ID claude-haiku-4-5-20251001.",
		"On October 21, 2025, Sanae Takaichi became Japan's first female prime minister.",
		"Donald Trump met Vladimir Putin in Alaska on August 15, 2025.",
		"The earthquake off Russia's Kamchatka Peninsula on July 30, 2025 had a moment magnitude of 8.8.",
		"The 2025 Nobel Peace Prize was awarded to Maria Corina Machado.",
		"Donald Trump is the 45th and 47th president of the United States.",
		"JD Vance is the vice president of the United States.",
		"The Milano Cortina 2026 Winter Olympics are scheduled for February 6-22, 2026.",
		"The 2026 FIFA World Cup is scheduled for June 11-July 19, 2026, hosted by Canada, Mexico, and the United States, and is the first 48-team FIFA World Cup.",
	}
	if profile != nil {
		facts = append([]string{
			fmt.Sprintf("For this request, the public model identity is %s.", profile.DisplayName),
			fmt.Sprintf("%s uses the Claude API model ID %s.", profile.DisplayName, profile.ExternalID),
			fmt.Sprintf("%s has a %s context window and supports up to %s output tokens.", profile.DisplayName, profile.ContextWindow, profile.MaxOutput),
			fmt.Sprintf("%s was released in %s.", profile.DisplayName, profile.ReleaseDate),
			fmt.Sprintf("%s has a reliable knowledge cutoff of %s and a training cutoff of %s.", profile.DisplayName, profile.KnowledgeCutoff, profile.TrainingCutoff),
			fmt.Sprintf("%s thinking/effort behavior: %s; %s.", profile.DisplayName, profile.ThinkingFeatures, profile.EffortFeatures),
		}, facts...)
	}
	return facts
}

func uniqueNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func parseAnthropicKiroKnowledgePack(raw string, profile *AnthropicKiroModelProfile) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return splitAnthropicKiroKnowledgeText(raw)
	}
	return uniqueNonEmptyStrings(extractAnthropicKiroKnowledgeFacts(value, profile))
}

func extractAnthropicKiroKnowledgeFacts(value any, profile *AnthropicKiroModelProfile) []string {
	var facts []string
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if fact, ok := item.(string); ok {
				facts = append(facts, fact)
			}
		}
	case map[string]any:
		facts = append(facts, extractAnthropicKiroKnowledgeFacts(v["facts"], profile)...)
		facts = append(facts, extractAnthropicKiroKnowledgeFacts(v["additional_facts"], profile)...)
		if profile != nil {
			if models, ok := v["models"].(map[string]any); ok {
				for _, key := range []string{profile.ExternalID, profile.KiroID, profile.DisplayName} {
					if modelValue, ok := models[key]; ok {
						facts = append(facts, anthropicKiroModelFactsFromConfiguredValue(profile, modelValue)...)
					}
				}
			}
		}
	case string:
		facts = append(facts, splitAnthropicKiroKnowledgeText(v)...)
	}
	return facts
}

func splitAnthropicKiroKnowledgeText(raw string) []string {
	var facts []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		if line != "" {
			facts = append(facts, line)
		}
	}
	return facts
}

func anthropicKiroModelFactsFromConfiguredValue(profile *AnthropicKiroModelProfile, value any) []string {
	obj, ok := value.(map[string]any)
	if !ok {
		return extractAnthropicKiroKnowledgeFacts(value, profile)
	}
	display := firstAnthropicKiroString(obj, "display_name", "display", "name")
	if display == "" {
		display = profile.DisplayName
	}
	externalID := firstAnthropicKiroString(obj, "external_id", "api_id", "model_id")
	if externalID == "" {
		externalID = profile.ExternalID
	}
	kiroID := firstAnthropicKiroString(obj, "kiro_id", "kiro", "internal_id")
	if kiroID == "" {
		kiroID = profile.KiroID
	}
	contextWindow := firstAnthropicKiroString(obj, "context_window", "context")
	maxOutput := firstAnthropicKiroString(obj, "max_output", "output")
	releaseDate := firstAnthropicKiroString(obj, "release_date", "released")
	knowledgeCutoff := firstAnthropicKiroString(obj, "knowledge_cutoff", "reliable_knowledge_cutoff")
	trainingCutoff := firstAnthropicKiroString(obj, "training_cutoff")
	thinking := firstAnthropicKiroString(obj, "thinking_features", "thinking")
	facts := []string{
		fmt.Sprintf("%s uses the Claude API model ID %s.", display, externalID),
	}
	_ = kiroID
	if contextWindow != "" || maxOutput != "" {
		facts = append(facts, fmt.Sprintf("%s has a %s context window and supports up to %s output tokens.", display, contextWindow, maxOutput))
	}
	if releaseDate != "" {
		facts = append(facts, fmt.Sprintf("%s was released in %s.", display, releaseDate))
	}
	if knowledgeCutoff != "" || trainingCutoff != "" {
		facts = append(facts, fmt.Sprintf("%s has a reliable knowledge cutoff of %s and a training cutoff of %s.", display, knowledgeCutoff, trainingCutoff))
	}
	if thinking != "" {
		facts = append(facts, fmt.Sprintf("%s thinking/effort behavior: %s.", display, thinking))
	}
	return facts
}

func firstAnthropicKiroString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func appendAnthropicKiroTextToMessage(msg map[string]any, text string) bool {
	switch content := msg["content"].(type) {
	case string:
		if strings.Contains(content, anthropicKiroRecentFactsMarker) {
			return false
		}
		msg["content"] = content + "\n\n" + text
		return true
	case []any:
		for _, part := range content {
			if obj, ok := part.(map[string]any); ok {
				if partText, _ := obj["text"].(string); strings.Contains(partText, anthropicKiroRecentFactsMarker) {
					return false
				}
			}
		}
		msg["content"] = append(content, map[string]any{"type": "text", "text": text})
		return true
	default:
		msg["content"] = []any{map[string]any{"type": "text", "text": text}}
		return true
	}
}

func convertAnthropicKiroPDFDocuments(payload map[string]any) bool {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, msgValue := range messages {
		msg, ok := msgValue.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for i, partValue := range parts {
			part, ok := partValue.(map[string]any)
			if !ok {
				continue
			}
			if !anthropicKiroIsPDFDocumentBlock(part) {
				continue
			}
			parts[i] = map[string]any{
				"type": "text",
				"text": anthropicKiroPDFDocumentText(part),
			}
			changed = true
		}
		if changed {
			msg["content"] = parts
		}
	}
	return changed
}

func anthropicKiroIsPDFDocumentBlock(part map[string]any) bool {
	partType, _ := part["type"].(string)
	if partType != "document" {
		return false
	}
	source, ok := part["source"].(map[string]any)
	if !ok {
		return false
	}
	sourceType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	return sourceType == "base64" && strings.EqualFold(mediaType, "application/pdf")
}

func anthropicKiroPDFDocumentText(part map[string]any) string {
	title := "document.pdf"
	for _, key := range []string{"title", "name", "filename"} {
		if v, ok := part[key].(string); ok && strings.TrimSpace(v) != "" {
			title = strings.TrimSpace(v)
			break
		}
	}
	source, _ := part["source"].(map[string]any)
	data, _ := source["data"].(string)
	text := extractAnthropicKiroPDFText(data)
	if strings.TrimSpace(text) == "" {
		text = "[PDF document could not be parsed]"
	}
	return fmt.Sprintf("[PDF Document: %s]\n%s\n[End of Document]", title, text)
}

func extractAnthropicKiroPDFText(encoded string) string {
	encoded = strings.TrimSpace(encoded)
	if idx := strings.Index(encoded, ","); strings.HasPrefix(strings.ToLower(encoded[:maxInt(idx, 0)]), "data:") && idx >= 0 {
		encoded = encoded[idx+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(raw) == 0 {
		return ""
	}

	chunks := [][]byte{raw}
	for _, match := range anthropicKiroPDFStreamPattern.FindAllSubmatch(raw, 64) {
		if len(match) < 2 {
			continue
		}
		stream := bytes.Trim(match[1], "\r\n ")
		if reader, err := zlib.NewReader(bytes.NewReader(stream)); err == nil {
			if decoded, readErr := io.ReadAll(io.LimitReader(reader, 4<<20)); readErr == nil && len(decoded) > 0 {
				chunks = append(chunks, decoded)
			}
			_ = reader.Close()
		}
	}

	seen := map[string]bool{}
	var out []string
	for _, chunk := range chunks {
		for _, text := range extractAnthropicKiroPDFTextOperators(string(chunk)) {
			text = strings.Join(strings.Fields(text), " ")
			if text == "" || seen[text] {
				continue
			}
			seen[text] = true
			out = append(out, text)
			if len(strings.Join(out, "\n")) > 20000 {
				break
			}
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func extractAnthropicKiroPDFTextOperators(data string) []string {
	var texts []string
	areas := anthropicKiroPDFBTETPattern.FindAllStringSubmatch(data, -1)
	if len(areas) == 0 {
		areas = [][]string{{"", data}}
	}
	for _, area := range areas {
		if len(area) < 2 {
			continue
		}
		for _, match := range anthropicKiroPDFLiteralPattern.FindAllString(area[1], -1) {
			texts = append(texts, decodeAnthropicKiroPDFLiteral(match))
		}
		for _, match := range anthropicKiroPDFHexPattern.FindAllStringSubmatch(area[1], -1) {
			if len(match) == 2 {
				texts = append(texts, decodeAnthropicKiroPDFHex(match[1]))
			}
		}
	}
	return texts
}

func decodeAnthropicKiroPDFLiteral(value string) string {
	if len(value) >= 2 && value[0] == '(' && value[len(value)-1] == ')' {
		value = value[1 : len(value)-1]
	}
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch != '\\' || i+1 >= len(value) {
			out.WriteByte(ch)
			continue
		}
		i++
		switch value[i] {
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'b':
			out.WriteByte('\b')
		case 'f':
			out.WriteByte('\f')
		case '\\', '(', ')':
			out.WriteByte(value[i])
		default:
			if value[i] >= '0' && value[i] <= '7' {
				octal := []byte{value[i]}
				for j := 0; j < 2 && i+1 < len(value) && value[i+1] >= '0' && value[i+1] <= '7'; j++ {
					i++
					octal = append(octal, value[i])
				}
				var b byte
				for _, digit := range octal {
					b = b*8 + (digit - '0')
				}
				out.WriteByte(b)
			} else {
				out.WriteByte(value[i])
			}
		}
	}
	return out.String()
}

func decodeAnthropicKiroPDFHex(value string) string {
	value = strings.Join(strings.Fields(value), "")
	if len(value)%2 == 1 {
		value += "0"
	}
	raw, err := hexToBytes(value)
	if err != nil || len(raw) == 0 {
		return ""
	}
	if len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF {
		u16 := make([]uint16, 0, (len(raw)-2)/2)
		for i := 2; i+1 < len(raw); i += 2 {
			u16 = append(u16, uint16(raw[i])<<8|uint16(raw[i+1]))
		}
		return string(utf16.Decode(u16))
	}
	return string(raw)
}

func hexToBytes(value string) ([]byte, error) {
	out := make([]byte, len(value)/2)
	for i := 0; i < len(out); i++ {
		hi, ok := fromHex(value[i*2])
		if !ok {
			return nil, fmt.Errorf("invalid hex")
		}
		lo, ok := fromHex(value[i*2+1])
		if !ok {
			return nil, fmt.Errorf("invalid hex")
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func fromHex(ch byte) (byte, bool) {
	switch {
	case ch >= '0' && ch <= '9':
		return ch - '0', true
	case ch >= 'a' && ch <= 'f':
		return ch - 'a' + 10, true
	case ch >= 'A' && ch <= 'F':
		return ch - 'A' + 10, true
	default:
		return 0, false
	}
}

func normalizeAnthropicKiroMessageObject(payload map[string]any, fallbackModel string, profile *AnthropicKiroModelProfile) bool {
	changed := false
	id, _ := payload["id"].(string)
	if !anthropicKiroMessageIDPattern.MatchString(id) {
		payload["id"] = generateAnthropicKiroMessageID()
		changed = true
	}
	if payload["type"] != "message" {
		payload["type"] = "message"
		changed = true
	}
	if payload["role"] != "assistant" {
		payload["role"] = "assistant"
		changed = true
	}
	outputModel := strings.TrimSpace(fallbackModel)
	if profile != nil && strings.TrimSpace(profile.ExternalID) != "" {
		outputModel = profile.ExternalID
	}
	if model, _ := payload["model"].(string); strings.TrimSpace(outputModel) != "" && model != outputModel {
		payload["model"] = outputModel
		changed = true
	}
	if _, ok := payload["content"].([]any); !ok {
		if text, ok := payload["content"].(string); ok {
			payload["content"] = []any{map[string]any{"type": "text", "text": text}}
		} else {
			payload["content"] = []any{}
		}
		changed = true
	}
	if _, ok := payload["stop_sequence"]; !ok {
		payload["stop_sequence"] = nil
		changed = true
	}
	if _, ok := payload["usage"].(map[string]any); !ok {
		payload["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
		changed = true
	}
	changed = normalizeAnthropicKiroThinkingBlocks(payload["content"]) || changed
	return changed
}

func normalizeAnthropicKiroThinkingBlocks(value any) bool {
	blocks, ok := value.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, blockValue := range blocks {
		block, ok := blockValue.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := block["type"].(string)
		if blockType != "thinking" && blockType != "redacted_thinking" {
			continue
		}
		if blockType == "thinking" {
			if _, ok := block["thinking"].(string); !ok {
				block["thinking"] = ""
				changed = true
			}
		}
	}
	return changed
}

func generateAnthropicKiroMessageID() string {
	return "msg_01" + randomAnthropicKiroBase32(22)
}

func generateAnthropicKiroRequestID() string {
	return "req_01" + randomAnthropicKiroBase32(22)
}

func normalizeAnthropicKiroRequestID(value string) string {
	if anthropicKiroRequestIDPattern.MatchString(value) {
		return value
	}
	return generateAnthropicKiroRequestID()
}

func randomAnthropicKiroBase32(n int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = alphabet[i%len(alphabet)]
		}
	} else {
		for i, b := range buf {
			buf[i] = alphabet[int(b)%len(alphabet)]
		}
	}
	return string(buf)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
