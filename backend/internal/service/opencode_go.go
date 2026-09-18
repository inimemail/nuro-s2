package service

import (
	"fmt"
	"math"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	openCodeGoProtocolRulesKey         = "protocol_rules"
	maxOpenCodeGoProtocolRules         = 64
	maxOpenCodeGoProtocolPatternLength = 128
	DefaultOpenCodeZenAnthropicBaseURL = "https://opencode.ai/zen/anthropic"
	DefaultOpenCodeGoAnthropicBaseURL  = "https://opencode.ai/zen/go/anthropic"
)

type OpenCodeGoProtocolRule struct {
	Pattern  string `json:"pattern"`
	Protocol string `json:"protocol"`
}

func (a *Account) GetOpenCodeAccountMode() string {
	if a == nil || !a.IsOpenCodeGo() {
		return ""
	}
	mode := strings.TrimSpace(a.GetCredential("account_mode"))
	if mode == "" {
		mode = strings.TrimSpace(a.getExtraString("account_mode"))
	}
	if strings.EqualFold(mode, AccountModeZen) {
		return AccountModeZen
	}
	return AccountModeGo
}

// GET /usage returns percentage windows, not token or money estimates.
func parseOpenCodeGoUsageTiers(body []byte) []CNQuotaTier {
	if !gjson.ValidBytes(body) {
		return nil
	}
	var tiers []CNQuotaTier
	for _, window := range []struct{ key, name string }{{"rolling", "5h"}, {"weekly", "weekly"}, {"monthly", "monthly"}} {
		node := gjson.GetBytes(body, "usage."+window.key)
		used, ok := cnParseF64(node.Get("percent").Value())
		if !ok || used < 0 || math.IsNaN(used) || math.IsInf(used, 0) {
			continue
		}
		tiers = append(tiers, CNQuotaTier{Window: window.name, UsedPercent: used, ResetAt: cnNormalizeResetTime(node.Get("resetsAt").Value())})
	}
	return tiers
}

func (a *Account) IsOpenCodeZen() bool { return a.GetOpenCodeAccountMode() == AccountModeZen }

func defaultOpenCodeProtocolRules(mode string) []OpenCodeGoProtocolRule {
	rules := []OpenCodeGoProtocolRule{
		{Pattern: "grok-*", Protocol: APIProtocolResponses},
		{Pattern: "gpt-*", Protocol: APIProtocolResponses},
		{Pattern: "muse-spark-*", Protocol: APIProtocolResponses},
		{Pattern: "qwen*", Protocol: APIProtocolAnthropic},
	}
	if mode == AccountModeZen {
		rules = append([]OpenCodeGoProtocolRule{{Pattern: "claude-*", Protocol: APIProtocolAnthropic}}, rules...)
	} else {
		rules = append(rules, OpenCodeGoProtocolRule{Pattern: "minimax-*", Protocol: APIProtocolAnthropic})
	}
	return rules
}

func normalizeOpenCodeModelID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"opencode-go/", "opencode_go/", "opencode/"} {
		model = strings.TrimPrefix(model, prefix)
	}
	return model
}

func normalizeOpenCodePattern(pattern string) (string, error) {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "" || len(pattern) > maxOpenCodeGoProtocolPatternLength || strings.ContainsAny(pattern, " \t\r\n") {
		return "", fmt.Errorf("pattern is empty, too long, or contains whitespace")
	}
	if strings.Count(pattern, "*") > 1 || (strings.Contains(pattern, "*") && !strings.HasSuffix(pattern, "*")) {
		return "", fmt.Errorf("pattern may use a single trailing * wildcard")
	}
	return pattern, nil
}

func parseOpenCodeProtocolRules(raw any) ([]OpenCodeGoProtocolRule, error) {
	items, ok := raw.([]any)
	if !ok {
		if typed, okTyped := raw.([]map[string]any); okTyped {
			items = make([]any, len(typed))
			for i := range typed {
				items[i] = typed[i]
			}
		} else {
			return nil, fmt.Errorf("protocol_rules must be an array")
		}
	}
	if len(items) > maxOpenCodeGoProtocolRules {
		return nil, fmt.Errorf("protocol_rules supports at most %d entries", maxOpenCodeGoProtocolRules)
	}
	rules := make([]OpenCodeGoProtocolRule, 0, len(items))
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("protocol_rules[%d] must be an object", i)
		}
		pattern, _ := entry["pattern"].(string)
		protocol, _ := entry["protocol"].(string)
		pattern, err := normalizeOpenCodePattern(pattern)
		if err != nil {
			return nil, fmt.Errorf("protocol_rules[%d]: %w", i, err)
		}
		protocol = strings.TrimSpace(protocol)
		if protocol != APIProtocolChatCompletions && protocol != APIProtocolAnthropic && protocol != APIProtocolResponses {
			return nil, fmt.Errorf("protocol_rules[%d]: unsupported protocol", i)
		}
		rules = append(rules, OpenCodeGoProtocolRule{Pattern: pattern, Protocol: protocol})
	}
	return rules, nil
}

func openCodePatternMatches(pattern, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = normalizeOpenCodeModelID(model)
	if pattern == "*" {
		return model != ""
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == model
}

func matchOpenCodeProtocolRules(model string, rules []OpenCodeGoProtocolRule) string {
	for _, rule := range rules {
		if openCodePatternMatches(rule.Pattern, model) {
			return rule.Protocol
		}
	}
	return APIProtocolChatCompletions
}

func (a *Account) ResolveOpenCodeGoUpstreamProtocol(model string) string {
	if a == nil || !a.IsOpenCodeGo() {
		return ""
	}
	configured := strings.TrimSpace(a.getExtraString(cnAPIProtocolExtraKey))
	if configured == "" {
		configured = strings.TrimSpace(a.GetCredential("api_protocol"))
	}
	if configured == APIProtocolChatCompletions || configured == APIProtocolAnthropic || configured == APIProtocolResponses {
		return configured
	}
	if raw, ok := a.Credentials[openCodeGoProtocolRulesKey]; ok && raw != nil {
		if rules, err := parseOpenCodeProtocolRules(raw); err == nil {
			return matchOpenCodeProtocolRules(model, rules)
		}
	}
	return matchOpenCodeProtocolRules(model, defaultOpenCodeProtocolRules(a.GetOpenCodeAccountMode()))
}

func NormalizeOpenCodeProtocolRulesCredentials(credentials map[string]any) error {
	if credentials == nil {
		return nil
	}
	raw, ok := credentials[openCodeGoProtocolRulesKey]
	if !ok || raw == nil {
		return nil
	}
	rules, err := parseOpenCodeProtocolRules(raw)
	if err != nil {
		return err
	}
	encoded := make([]any, len(rules))
	for i, rule := range rules {
		encoded[i] = map[string]any{"pattern": rule.Pattern, "protocol": rule.Protocol}
	}
	credentials[openCodeGoProtocolRulesKey] = encoded
	return nil
}
