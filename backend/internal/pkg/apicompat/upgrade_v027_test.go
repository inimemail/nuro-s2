package apicompat

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"testing"
)

func TestV027RootSchemaPreservesConstraints(t *testing.T) {
	for _, raw := range []string{
		`{"allOf":[{"type":"object","properties":{"n":{"minimum":9007199254740993}},"required":["n"]},{"type":"object","properties":{"n":{"maximum":9007199254740994}}}]}`,
		`{"oneOf":[{"type":"object","properties":{"mode":{"const":"a"}},"required":["mode"],"additionalProperties":false},{"type":"object","properties":{"mode":{"const":"b"}},"required":["mode"],"additionalProperties":false}]}`,
	} {
		out, err := normalizeAnthropicRootSchema([]byte(raw))
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(out, "allOf").Exists())
		require.False(t, gjson.GetBytes(out, "oneOf").Exists())
		if gjson.Get(raw, "allOf").Exists() {
			require.Contains(t, string(out), "9007199254740993")
			require.True(t, gjson.GetBytes(out, "properties.n.allOf").IsArray())
		} else {
			require.True(t, gjson.GetBytes(out, "properties.mode.oneOf").IsArray())
			require.Equal(t, "false", gjson.GetBytes(out, "additionalProperties").Raw)
		}
	}
	for _, raw := range []string{
		`{"anyOf":[{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}},{"type":"object","required":["b"],"properties":{"b":{"type":"string"}}}]}`,
		`{"allOf":[{"type":"object","additionalProperties":false,"properties":{"a":{}}},{"type":"object","properties":{"b":{}}}]}`,
		`{"oneOf":[{"type":"object","properties":{"a":{"const":1}}},{"type":"object","properties":{"a":{"const":2}}}]}`,
		`{"anyOf":[true,{"type":"object"}]}`,
	} {
		_, err := normalizeAnthropicRootSchema([]byte(raw))
		require.Error(t, err, raw)
	}
}

func TestV027ToolMediaDoesNotMutateAndIsIdempotent(t *testing.T) {
	raw := `[{"type":"function_call_output","call_id":"a","output":[{"type":"input_image","image_url":"https://image.example/a"}]},{"role":"developer","content":"notice"},{"type":"function_call_output","call_id":"b","output":"done"}]`
	var input any
	require.NoError(t, json.Unmarshal([]byte(raw), &input))
	before, _ := json.Marshal(input)
	out, changed := LiftResponsesToolOutputMedia(input)
	require.True(t, changed)
	after, _ := json.Marshal(input)
	require.Equal(t, before, after)
	encoded, _ := json.Marshal(out)
	require.Equal(t, "b", gjson.GetBytes(encoded, "1.call_id").String())
	require.Equal(t, "user", gjson.GetBytes(encoded, "2.role").String())
	require.Equal(t, "developer", gjson.GetBytes(encoded, "3.role").String())
	second, changed := LiftResponsesToolOutputMedia(out)
	require.False(t, changed)
	require.Equal(t, out, second)
}
