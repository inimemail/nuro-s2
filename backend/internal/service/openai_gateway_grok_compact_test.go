package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokCompactRequestAndResponseRoundTrip(t *testing.T) {
	body, err := buildGrokCompactRequestBody([]byte(`{"model":"grok-4","input":"hello","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, "grok-4", gjson.GetBytes(body, "model").String())
	require.False(t, gjson.GetBytes(body, "stream").Bool())
	require.Equal(t, "none", gjson.GetBytes(body, "tool_choice").String())

	response, err := convertGrokResponseToOpenAICompact([]byte(`{"id":"resp_1","output":[{"type":"reasoning","encrypted_content":"enc"},{"type":"message","content":[{"type":"output_text","text":"summary"}]}]}`))
	require.NoError(t, err)
	require.Equal(t, "compaction", gjson.GetBytes(response, "output.0.type").String())
	require.Equal(t, "enc", gjson.GetBytes(response, "output.0.encrypted_content").String())
	require.Equal(t, "summary", gjson.GetBytes(response, "output.0.summary.0.text").String())
}

func TestGrokContentPolicyDoesNotFailoverOrCooldown(t *testing.T) {
	body := []byte(`{"error":{"code":"content_policy_violation","message":"prompt violates policy"}}`)
	require.True(t, isGrokContentPolicyRejection(http.StatusForbidden, body))
	require.False(t, (&OpenAIGatewayService{}).shouldFailoverGrokUpstreamError(http.StatusForbidden, body))
}
