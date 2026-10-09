package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIResponsesNamespaceNamesContextKey = "openai_responses_namespace_names"

func shouldFlattenOpenAIResponsesNamespaces(account *Account, transport OpenAIUpstreamTransport, passthroughEnabled bool, compactPath bool) bool {
	// Automatic passthrough promises to preserve the request and response
	// payloads, with authentication replacement as the only transformation.
	// Namespace flattening is a compatibility transform for the legacy OAuth
	// HTTP path and must never run on the strict passthrough path.
	if passthroughEnabled {
		return false
	}
	if account == nil || !account.IsOpenAIOAuthLike() {
		return false
	}
	if !compactPath && !account.IsOpenAIResponsesFlattenNamespacesEnabled() {
		return false
	}
	if transport == OpenAIUpstreamTransportResponsesWebsocketV2 && !passthroughEnabled {
		return false
	}
	return true
}

// shouldStripOpenAIResponsesInputNamespaces removes residual direct input item
// namespaces for OpenAI OAuth and API Key HTTP forwarding. Native WSv2 keeps
// namespace semantics unless strict passthrough selected the HTTP-compatible
// payload contract.
func shouldStripOpenAIResponsesInputNamespaces(account *Account, transport OpenAIUpstreamTransport, passthroughEnabled bool) bool {
	// The local strict-passthrough contract preserves the payload apart from
	// its separately documented authentication/store/stream policies. Keep the
	// upstream compatibility cleanup on transformed HTTP requests only.
	if passthroughEnabled {
		return false
	}
	if account == nil || (!account.IsOpenAIOAuthLike() && !account.IsOpenAIApiKey()) {
		return false
	}
	return transport != OpenAIUpstreamTransportResponsesWebsocketV2
}

// shouldKeepOpenAIResponsesToolCallNamespaces keeps namespace metadata on
// historical tool-call items only when the selected upstream understands the
// namespace extension. Responses Lite carries declarations in input items, so
// those declarations are included in the same decision.
func shouldKeepOpenAIResponsesToolCallNamespaces(account *Account, transport OpenAIUpstreamTransport, passthroughEnabled bool, compactPath bool, body []byte) bool {
	if account == nil || compactPath {
		return false
	}
	if account.IsOpenAIApiKey() {
		return hasOpenAIResponsesNamespaceToolDeclaration(body)
	}
	if !account.IsOpenAIOAuthLike() {
		return false
	}
	return !shouldFlattenOpenAIResponsesNamespaces(account, transport, passthroughEnabled, compactPath)
}

func hasOpenAIResponsesNamespaceToolDeclaration(body []byte) bool {
	hasNamespaceTool := func(tools gjson.Result) bool {
		if !tools.IsArray() {
			return false
		}
		found := false
		tools.ForEach(func(_, tool gjson.Result) bool {
			if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "namespace") {
				found = true
				return false
			}
			return true
		})
		return found
	}
	if hasNamespaceTool(gjson.GetBytes(body, "tools")) {
		return true
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if !strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "additional_tools") {
			return true
		}
		if hasNamespaceTool(item.Get("tools")) {
			found = true
			return false
		}
		return true
	})
	return found
}

func flattenOpenAIResponsesNamespaces(c *gin.Context, body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"namespace"`)) {
		return body, nil
	}
	var requestBody map[string]any
	if err := json.Unmarshal(body, &requestBody); err != nil {
		return body, fmt.Errorf("decode OpenAI namespace body: %w", err)
	}
	names, changed, err := apicompat.FlattenResponsesNamespacesExcept(requestBody, map[string]bool{"image_gen": true})
	if err != nil {
		return body, err
	}
	if !changed {
		return body, nil
	}
	rebuilt, err := json.Marshal(requestBody)
	if err != nil {
		return body, fmt.Errorf("encode OpenAI namespace body: %w", err)
	}
	if c != nil {
		c.Set(openAIResponsesNamespaceNamesContextKey, names)
	}
	return rebuilt, nil
}

// stripOpenAIResponsesInputNamespaces removes only direct input item
// namespaces. It keeps nested fields and numeric JSON tokens byte-exact.
var openAIResponsesToolCallItemTypes = map[string]bool{
	"function_call": true, "tool_call": true, "custom_tool_call": true, "mcp_tool_call": true,
}

func stripOpenAIResponsesInputNamespaces(body []byte, keepNamespaces ...bool) ([]byte, error) {
	keepToolCallNamespaces := len(keepNamespaces) > 0 && keepNamespaces[0]
	if !bytes.Contains(body, []byte(`"namespace"`)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	var rebuilt []byte
	lastEnd := 0
	var stripErr error
	input.ForEach(func(_, item gjson.Result) bool {
		if item.IsObject() && item.Get("namespace").Exists() &&
			(!keepToolCallNamespaces || !openAIResponsesToolCallItemTypes[strings.ToLower(strings.TrimSpace(item.Get("type").String()))]) {
			start, end := item.Index, item.Index+len(item.Raw)
			if start < lastEnd || end > len(body) || !bytes.Equal(body[start:end], []byte(item.Raw)) {
				stripErr = fmt.Errorf("invalid input item span")
				return false
			}
			itemBody, err := sjson.DeleteBytes(body[start:end], "namespace")
			stripErr = err
			if stripErr != nil {
				return false
			}
			if rebuilt == nil {
				rebuilt = make([]byte, 0, len(body))
			}
			rebuilt = append(rebuilt, body[lastEnd:start]...)
			rebuilt = append(rebuilt, itemBody...)
			lastEnd = end
		}
		return true
	})
	if stripErr != nil {
		return body, fmt.Errorf("delete OpenAI input namespace: %w", stripErr)
	}
	if rebuilt == nil {
		return body, nil
	}
	rebuilt = append(rebuilt, body[lastEnd:]...)
	return rebuilt, nil
}

func clearOpenAIResponsesNamespaceNames(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(openAIResponsesNamespaceNamesContextKey, map[string]apicompat.ResponsesNamespaceName(nil))
}

func restoreOpenAIResponsesNamespacePayload(c *gin.Context, payload []byte) ([]byte, error) {
	if c == nil || !json.Valid(payload) {
		return payload, nil
	}
	value, ok := c.Get(openAIResponsesNamespaceNamesContextKey)
	if !ok {
		return payload, nil
	}
	names, ok := value.(map[string]apicompat.ResponsesNamespaceName)
	if !ok || len(names) == 0 {
		return payload, nil
	}
	restored, _, err := apicompat.RestoreResponsesNamespaceCalls(payload, names)
	return restored, err
}
