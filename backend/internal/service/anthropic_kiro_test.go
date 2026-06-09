package service

import (
	"strings"
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
		system := gjson.GetBytes(updated, "system").String()
		require.Contains(t, system, anthropicKiroIdentityGuardMarker)
		require.Contains(t, system, "verified_recent_facts")
	})

	t.Run("prepends text block to array system", func(t *testing.T) {
		body := []byte(`{"system":[{"type":"text","text":"Existing system"}],"messages":[]}`)
		updated := injectAnthropicKiroIdentityGuard(body)
		require.Contains(t, gjson.GetBytes(updated, "system.0.text").String(), anthropicKiroIdentityGuardMarker)
		require.Equal(t, "Existing system", gjson.GetBytes(updated, "system.1.text").String())
	})
}

func TestPrepareAnthropicKiroRequestBody(t *testing.T) {
	t.Run("adds structured output and recent facts", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"Return data."}],"response_format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}}`)
		updated := prepareAnthropicKiroRequestBody(body, true, nil, nil)

		require.Contains(t, gjson.GetBytes(updated, "system").String(), anthropicKiroStructuredMarker)
		require.Contains(t, gjson.GetBytes(updated, "messages.0.content").String(), "verified_recent_facts")
		require.Equal(t, "answer", gjson.GetBytes(updated, "response_format.json_schema.name").String())
		require.Equal(t, "object", gjson.GetBytes(updated, "response_format.json_schema.schema.type").String())
	})

	t.Run("converts pdf document blocks to text", func(t *testing.T) {
		pdf := "JVBERi0xLjQKMSAwIG9iago8Pj4Kc3RyZWFtCkJUCihIZWxsbyBmcm9tIFBERikgVGoKRVQKZW5kc3RyZWFtCmVuZG9iago="
		body := []byte(`{"messages":[{"role":"user","content":[{"type":"document","title":"paper.pdf","source":{"type":"base64","media_type":"application/pdf","data":"` + pdf + `"}}]}]}`)
		updated := prepareAnthropicKiroRequestBody(body, true, nil, nil)
		text := gjson.GetBytes(updated, "messages.0.content.0.text").String()

		require.Equal(t, "text", gjson.GetBytes(updated, "messages.0.content.0.type").String())
		require.Contains(t, text, "[PDF Document: paper.pdf]")
		require.Contains(t, text, "Hello from PDF")
		require.Contains(t, text, "[End of Document]")
	})

	t.Run("can skip identity guard for request-compatible preprocessing", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"Count this."}]}`)
		updated := prepareAnthropicKiroRequestBody(body, false, nil, nil)

		require.False(t, gjson.GetBytes(updated, "system").Exists())
		require.NotContains(t, string(updated), anthropicKiroIdentityGuardMarker)
		require.Contains(t, gjson.GetBytes(updated, "messages.0.content").String(), "verified_recent_facts")
	})

	t.Run("maps external model to kiro model and injects model profile facts", func(t *testing.T) {
		profile := resolveAnthropicKiroModelProfile("claude-opus-4-8")
		body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"Who are you?"}]}`)
		updated := prepareAnthropicKiroRequestBody(body, true, profile, []string{"Configured fact from auto refresh."})

		require.Equal(t, "claude-opus-4.8", gjson.GetBytes(updated, "model").String())
		require.Contains(t, gjson.GetBytes(updated, "system").String(), "Your public model identity is Claude Opus 4.8.")
		content := gjson.GetBytes(updated, "messages.0.content").String()
		require.Contains(t, content, "Claude Opus 4.8 uses the Claude API model ID claude-opus-4-8.")
		require.Contains(t, content, "Configured fact from auto refresh.")
	})
}

func TestNormalizeAnthropicKiroMessagePayload(t *testing.T) {
	body := []byte(`{"id":"bad","role":"assistant","model":"claude-opus-4.8","content":[{"type":"thinking","thinking":"checking"},{"type":"text","text":"I am Kiro and my model is Claude Sonnet 4.5. Model: claude-sonnet-4.5"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))

	require.Regexp(t, anthropicKiroMessageIDPattern, gjson.Get(updated, "id").String())
	require.Equal(t, "message", gjson.Get(updated, "type").String())
	require.Equal(t, "claude-opus-4-8", gjson.Get(updated, "model").String())
	require.NotEmpty(t, gjson.Get(updated, "content.0.signature").String())
	require.Contains(t, gjson.Get(updated, "content.1.text").String(), "Claude Opus 4.8")
	require.Contains(t, gjson.Get(updated, "content.1.text").String(), "claude-opus-4-8")
	require.NotContains(t, gjson.Get(updated, "content.1.text").String(), "Claude Sonnet 4.5")
	require.True(t, gjson.Get(updated, "stop_sequence").Exists())
}

func TestNormalizeAnthropicKiroMessagePayloadWithRequestID(t *testing.T) {
	body := []byte(`{"id":"bad","role":"assistant","content":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	updated := string(normalizeAnthropicKiroMessagePayloadWithRequestID(body, "claude-sonnet-4-5-20250929", "raw-request"))

	require.Regexp(t, anthropicKiroMessageIDPattern, gjson.Get(updated, "id").String())
	require.Regexp(t, anthropicKiroRequestIDPattern, gjson.Get(updated, "request_id").String())
}

func TestAnthropicKiroSSENormalizer(t *testing.T) {
	n := newAnthropicKiroSSENormalizer("claude-opus-4-8", resolveAnthropicKiroModelProfile("claude-opus-4-8"))

	lines := n.normalizeLine(`data: {"type":"message_start","message":{"id":"raw","role":"assistant","model":"claude-opus-4.8","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)
	require.Len(t, lines, 2)
	require.Equal(t, "event: message_start", lines[0])
	require.Regexp(t, anthropicKiroMessageIDPattern, gjson.Get(strings.TrimPrefix(lines[1], "data: "), "message.id").String())
	require.Equal(t, "message", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "message.type").String())
	require.Equal(t, "claude-opus-4-8", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "message.model").String())

	lines = n.normalizeLine(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"x"}}`)
	require.Len(t, lines, 2)
	lines = n.normalizeLine(`data: {"type":"content_block_stop","index":0}`)
	require.Len(t, lines, 5)
	require.Equal(t, "event: content_block_delta", lines[0])
	require.Equal(t, "", lines[2])
	require.Equal(t, "event: content_block_stop", lines[3])
	require.Equal(t, `data: {"type":"content_block_stop","index":0}`, lines[4])
	require.Equal(t, "signature_delta", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "delta.type").String())
	require.NotEmpty(t, gjson.Get(strings.TrimPrefix(lines[1], "data: "), "delta.signature").String())
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
