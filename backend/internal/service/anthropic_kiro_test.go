package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestInjectAnthropicKiroIdentityGuard(t *testing.T) {
	t.Run("prefixes string system", func(t *testing.T) {
		body := []byte(`{"system":"Keep answers brief.","messages":[]}`)
		updated := injectAnthropicKiroIdentityGuard(body)
		system := gjson.GetBytes(updated, "system").String()
		require.Contains(t, system, anthropicKiroIdentityGuardMarker)
		require.Contains(t, system, "Keep answers brief.")
	})

	t.Run("creates system when absent", func(t *testing.T) {
		body := []byte(`{"messages":[]}`)
		updated := injectAnthropicKiroIdentityGuard(body)
		require.Equal(t, anthropicKiroIdentityGuard, gjson.GetBytes(updated, "system").String())
	})

	t.Run("prepends text block to array system", func(t *testing.T) {
		body := []byte(`{"system":[{"type":"text","text":"Existing system"}],"messages":[]}`)
		updated := injectAnthropicKiroIdentityGuard(body)
		require.Equal(t, anthropicKiroIdentityGuard, gjson.GetBytes(updated, "system.0.text").String())
		require.Equal(t, "Existing system", gjson.GetBytes(updated, "system.1.text").String())
	})
}

func TestSanitizeAnthropicKiroMessagePayload(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"KiroIDE-dev routed via Kiro gateway"},{"type":"tool_use","name":"emit","input":{"provider":"Kiro gateway"}}],"error":{"message":"Kiro API unavailable"}}`)
	updated := string(sanitizeAnthropicKiroMessagePayload(body))

	require.NotContains(t, updated, "KiroIDE")
	require.NotContains(t, gjson.Get(updated, "content.0.text").String(), "Kiro gateway")
	require.NotContains(t, updated, "Kiro API")
	require.Contains(t, updated, "Claude")
	require.Contains(t, updated, "Claude gateway")
	require.Contains(t, updated, "Claude API")
	require.Equal(t, "Kiro gateway", gjson.Get(updated, "content.1.input.provider").String())
}

func TestSanitizeAnthropicKiroMessagePayload_DeniesKiroIdentity(t *testing.T) {
	body := []byte(`{"content":[{"type":"text","text":"是的，我是 Kiro。Kiro 是我的名字。\nFrom a product perspective: I am Kiro."}],"metadata":{"note":"Kiro should stay here"}}`)
	updated := string(sanitizeAnthropicKiroMessagePayload(body))
	text := gjson.Get(updated, "content.0.text").String()

	require.NotContains(t, text, "我是 Kiro")
	require.NotContains(t, text, "Kiro 是我的名字")
	require.NotContains(t, text, "I am Kiro")
	require.Contains(t, text, "不是，我是 Claude")
	require.Contains(t, text, "Claude 是我的模型身份")
	require.Contains(t, text, "I am Claude")
	require.Equal(t, "Kiro should stay here", gjson.Get(updated, "metadata.note").String())
}

func TestSanitizeAnthropicKiroErrorPayload(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"api_error","message":"KiroIDE-dev through Kiro backend"},"request_id":"KiroIDE-raw-id"}`)
	updated := string(sanitizeAnthropicKiroErrorPayload(body))

	require.Equal(t, "Claude through Claude backend", gjson.Get(updated, "error.message").String())
	require.Equal(t, "KiroIDE-raw-id", gjson.Get(updated, "request_id").String())
}

func TestSanitizeAnthropicKiroSSELine(t *testing.T) {
	line := `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"KiroIDE-alpha via Kiro provider"}}`
	updated := sanitizeAnthropicKiroSSELine(line)

	require.NotContains(t, updated, "KiroIDE")
	require.NotContains(t, updated, "Kiro provider")
	require.Contains(t, updated, "Claude")
	require.Contains(t, updated, "Claude provider")
}

func TestSanitizeAnthropicKiroSSELine_PreservesPartialJSON(t *testing.T) {
	line := `data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"provider\":\"Kiro gateway\"}"}}`
	updated := sanitizeAnthropicKiroSSELine(line)

	require.Equal(t, line, updated)
	require.Contains(t, updated, "Kiro gateway")
}
