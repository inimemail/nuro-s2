package service

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

const openAIResponsesNamespaceNamesContextKey = "openai_responses_namespace_names"

func shouldFlattenOpenAIResponsesNamespaces(account *Account, transport OpenAIUpstreamTransport, passthroughEnabled bool) bool {
	// Automatic passthrough promises to preserve the request and response
	// payloads, with authentication replacement as the only transformation.
	// Namespace flattening is a compatibility transform for the legacy OAuth
	// HTTP path and must never run on the strict passthrough path.
	if passthroughEnabled {
		return false
	}
	if account == nil || account.Type != AccountTypeOAuth {
		return false
	}
	if transport == OpenAIUpstreamTransportResponsesWebsocketV2 && !passthroughEnabled {
		return false
	}
	return true
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
