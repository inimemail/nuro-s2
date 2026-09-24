package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// strictChatCompatibility is scoped to the selected destination, not model names.
// Grok/OAuth/OpenCode keep their own transport and protocol semantics.
func strictChatCompatibility(account *Account, targetURL string) (roles, reasoning bool) {
	if account == nil || account.Type != AccountTypeAPIKey || account.IsGrok() || account.IsOpenCodeGo() {
		return false, false
	}
	switch account.Platform {
	case PlatformDeepSeek:
		return true, true
	case PlatformKimi, PlatformZhipu:
		return true, false
	case PlatformOpenAI:
		parsed, err := url.Parse(targetURL)
		if err != nil {
			return false, false
		}
		switch strings.ToLower(parsed.Hostname()) {
		case "api.deepseek.com":
			return true, true
		case "api.kimi.com", "api.moonshot.cn", "api.moonshot.ai", "open.bigmodel.cn", "api.z.ai":
			return true, false
		}
	}
	return false, false
}

// normalizeStrictChatRequest preserves real reasoning, unknown fields and exact
// numbers. Non-target destinations and unchanged requests return the input bytes.
func normalizeStrictChatRequest(account *Account, targetURL string, body []byte) ([]byte, error) {
	roles, reasoning := strictChatCompatibility(account, targetURL)
	if !roles && !reasoning {
		return body, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, fmt.Errorf("invalid chat request object")
	}
	raw, exists := root["messages"]
	if !exists {
		return body, nil
	}
	var messages []json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return nil, fmt.Errorf("invalid chat messages array")
	}
	changed := false
	for i, rawMessage := range messages {
		var message map[string]json.RawMessage
		if json.Unmarshal(rawMessage, &message) != nil || message == nil {
			return nil, fmt.Errorf("invalid chat message object")
		}
		var role string
		if json.Unmarshal(message["role"], &role) != nil {
			return nil, fmt.Errorf("invalid chat message role")
		}
		modified := false
		if roles && role == "developer" {
			message["role"] = json.RawMessage(`"system"`)
			modified = true
		}
		if reasoning && role == "assistant" {
			value, found := message["reasoning_content"]
			var content string
			if !found || string(value) == "null" || (json.Unmarshal(value, &content) == nil && content == "") {
				message["reasoning_content"] = json.RawMessage(`" "`)
				modified = true
			}
		}
		if modified {
			encoded, err := json.Marshal(message)
			if err != nil {
				return nil, err
			}
			messages[i] = encoded
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	root["messages"] = encoded
	return json.Marshal(root)
}

// Keep the protected raw transport unchanged. This wrapper is entered only once
// the dispatcher has selected Chat Completions, including explicit fallbacks.
func (s *OpenAIGatewayService) forwardAsCompatibleRawChatCompletions(ctx context.Context, c *gin.Context, account *Account, body []byte, defaultMappedModel string) (*OpenAIForwardResult, error) {
	if account != nil {
		original := gjson.GetBytes(body, "model").String()
		model := normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, original, defaultMappedModel))
		if err := validateNewModelChatReasoningTools(model, gjson.GetBytes(body, "reasoning_effort").String(), len(gjson.GetBytes(body, "tools").Array()) > 0 || len(gjson.GetBytes(body, "functions").Array()) > 0); err != nil {
			writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return nil, err
		}
		base := account.GetOpenAIBaseURL()
		if base == "" {
			base = "https://api.openai.com"
		}
		// The raw transport validates this same base URL before any network IO.
		var err error
		body, err = normalizeStrictChatRequest(account, buildOpenAIChatCompletionsURL(base), body)
		if err != nil {
			writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return nil, err
		}
	}
	return s.forwardAsRawChatCompletions(ctx, c, account, body, defaultMappedModel)
}

func validateNewModelChatReasoningTools(model, effort string, hasTools bool) error {
	if (openai.IsGPT6SolOrLunaModelSpelling(model) || claude.IsOpus55(model)) && hasTools && effort != "none" {
		return fmt.Errorf("model %s requires the Responses or native Messages endpoint for tools with reasoning", model)
	}
	return nil
}
