package service

import (
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAINativeCompactionV2Key = "openai_native_compaction_v2"

// MarkOpenAINativeCompactionV2 records only the request classification, never payload data.
func MarkOpenAINativeCompactionV2(c *gin.Context) {
	if c != nil {
		c.Set(openAINativeCompactionV2Key, true)
	}
}

func IsOpenAINativeCompactionV2(c *gin.Context) bool {
	return c != nil && c.GetBool(openAINativeCompactionV2Key)
}

// HasCompactionTriggerInInput detects Codex remote compact v2 requests that
// carry the compact signal in a normal /v1/responses body.
func HasCompactionTriggerInInput(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "compaction_trigger" {
			found = true
			return false
		}
		return true
	})
	return found
}
