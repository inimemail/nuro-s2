package service

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Partition only explicit thread/side-request identities. Ordinary sessions
// retain the local sticky key and strong-isolation callers never use this key.
func resolveOpenAIWSExecutionScope(c *gin.Context, body []byte, apiKeyID int64) string {
	type turnMetadata struct {
		ThreadID    string `json:"thread_id"`
		RequestKind string `json:"request_kind"`
	}
	var metadata turnMetadata
	raw := ""
	if c != nil && c.Request != nil {
		raw = c.GetHeader(openAIWSTurnMetadataHeader)
	}
	if raw == "" || json.Unmarshal([]byte(raw), &metadata) != nil {
		metadata = turnMetadata{}
		raw = gjson.GetBytes(body, "client_metadata."+openAIWSTurnMetadataHeader).String()
		if json.Unmarshal([]byte(raw), &metadata) != nil {
			metadata = turnMetadata{}
		}
	}
	thread := ""
	if c != nil && c.Request != nil {
		thread = strings.TrimSpace(c.GetHeader("thread-id"))
	}
	if thread == "" {
		thread = strings.TrimSpace(metadata.ThreadID)
	}
	lane := strings.ToLower(strings.TrimSpace(metadata.RequestKind))
	switch lane {
	case "", "turn", "prewarm", "compaction":
		lane = ""
	}
	if lane == "" && metadata.ThreadID == "" {
		subagent := ""
		if c != nil && c.Request != nil {
			subagent = c.GetHeader("x-openai-subagent")
		}
		if subagent == "" {
			subagent = gjson.GetBytes(body, "client_metadata.x-openai-subagent").String()
		}
		if subagent = strings.TrimSpace(subagent); subagent != "" {
			lane = "subagent=" + strings.ToLower(subagent)
		}
	}
	if thread == "" && lane == "" {
		return ""
	}
	identity := thread
	if identity == "" {
		identity = explicitOpenAIRequestSessionID(c, body)
	}
	if identity == "" {
		return ""
	}
	seed, _ := json.Marshal([]any{"openai_ws_exec", apiKeyID, identity, lane})
	scope, _ := deriveOpenAISessionHashes(string(seed))
	return scope
}

func openAIWSExecutionScopeBody(req map[string]any) []byte {
	subset := make(map[string]any, 2)
	for _, key := range []string{"client_metadata", "prompt_cache_key"} {
		if value, ok := req[key]; ok {
			subset[key] = value
		}
	}
	body, _ := json.Marshal(subset)
	return body
}
