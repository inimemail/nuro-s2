package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestShouldSanitizeOpenAIResponsesInputItemIDs(t *testing.T) {
	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.True(t, shouldSanitizeOpenAIResponsesInputItemIDs(apiKey, false))
	require.False(t, shouldSanitizeOpenAIResponsesInputItemIDs(apiKey, true), "strict passthrough must preserve input IDs")
	require.False(t, shouldSanitizeOpenAIResponsesInputItemIDs(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, false))
	require.False(t, shouldSanitizeOpenAIResponsesInputItemIDs(nil, false))
}

func TestSanitizeOpenAIResponsesInputItemIDs(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","id":"item_local"},{"type":"message","id":"msg_real"},{"type":"function_call","id":"item_call"},{"type":"custom_tool_call","id":"fc_real"},{"type":"function_call_output","id":"item_output"},{"type":"unknown","id":"item_keep"},{"type":"reasoning","id":"item_reasoning","encrypted_content":"keep"},{"type":"reasoning","id":"rs_real"}],"seed":9007199254740993}`)
	got, changed, err := sanitizeOpenAIResponsesInputItemIDs(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(got, "input.0.id").Exists())
	require.Equal(t, "msg_real", gjson.GetBytes(got, "input.1.id").String())
	require.False(t, gjson.GetBytes(got, "input.2.id").Exists())
	require.Equal(t, "fc_real", gjson.GetBytes(got, "input.3.id").String())
	require.Equal(t, "item_output", gjson.GetBytes(got, "input.4.id").String())
	require.Equal(t, "item_keep", gjson.GetBytes(got, "input.5.id").String())
	require.False(t, gjson.GetBytes(got, "input.6.id").Exists())
	require.Equal(t, "keep", gjson.GetBytes(got, "input.6.encrypted_content").String())
	require.Equal(t, "rs_real", gjson.GetBytes(got, "input.7.id").String())
	require.Equal(t, gjson.GetBytes(body, "seed").Raw, gjson.GetBytes(got, "seed").Raw)
}

func TestSanitizeOpenAIResponsesInputItemIDsNoop(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"input":"hello"}`),
		[]byte(`{"input":[{"type":"message","id":"msg_real"}]}`),
		[]byte(`{"input":[{"type":"message","id":42}]}`),
	} {
		got, changed, err := sanitizeOpenAIResponsesInputItemIDs(body)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, got)
	}
}
