package service

import (
	"bytes"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const responsesInstructionsEchoKey = "nuro.responses.instructions.echo"

// Attempt-local presentation metadata. It never changes the upstream prompt.
type responsesInstructionsEcho struct {
	raw     string
	restore bool
}

func rememberOpenAIResponsesInstructions(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	v := gjson.GetBytes(body, "instructions")
	raw := ""
	if v.Exists() {
		var b bytes.Buffer
		if json.Compact(&b, []byte(v.Raw)) == nil {
			raw = b.String()
		}
	}
	c.Set(responsesInstructionsEchoKey, responsesInstructionsEcho{raw: raw})
}

func syncOpenAIResponsesInstructions(c *gin.Context, upstream []byte) {
	if c == nil {
		return
	}
	v, ok := c.Get(responsesInstructionsEchoKey)
	echo, valid := v.(responsesInstructionsEcho)
	if !ok || !valid {
		return
	}
	value := gjson.GetBytes(upstream, "instructions")
	echo.restore = echo.raw != value.Raw
	if echo.raw != "" && value.Type == gjson.String && gjson.Parse(echo.raw).Type == gjson.String {
		echo.restore = gjson.Parse(echo.raw).String() != value.String()
	}
	c.Set(responsesInstructionsEchoKey, echo)
}

func restoreOpenAIResponsesInstructions(c *gin.Context, body []byte, path string) []byte {
	if c == nil {
		return body
	}
	v, _ := c.Get(responsesInstructionsEchoKey)
	echo, ok := v.(responsesInstructionsEcho)
	if !ok || !echo.restore || !gjson.GetBytes(body, path).Exists() {
		return body
	}
	var out []byte
	var err error
	if echo.raw == "" {
		out, err = sjson.DeleteBytes(body, path)
	} else {
		out, err = sjson.SetRawBytes(body, path, []byte(echo.raw))
	}
	if err != nil {
		return body
	}
	return out
}
