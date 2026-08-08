package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeOpenAIResponsesToolParameterTypes(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"parameters":{"type":null}}}],"input":[{"tools":[{"parameters":{"type":null}}]}]}`)
	got, changed, err := sanitizeOpenAIResponsesToolParameterTypes(body)
	require.NoError(t, err)
	require.True(t, changed)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Equal(t, "object", decoded["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)["type"])
	require.Equal(t, "object", decoded["input"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)["parameters"].(map[string]any)["type"])
}

func TestSanitizeOpenAIResponsesToolParameterTypesLeavesMissingType(t *testing.T) {
	body := []byte(`{"tools":[{"parameters":{}}]}`)
	got, changed, err := sanitizeOpenAIResponsesToolParameterTypes(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(got))
}
