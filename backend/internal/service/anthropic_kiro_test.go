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

		system := gjson.GetBytes(updated, "system").String()
		require.Contains(t, system, anthropicKiroStructuredMarker)
		require.Contains(t, system, "verified_recent_facts")
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

		require.True(t, gjson.GetBytes(updated, "system").Exists())
		require.NotContains(t, string(updated), anthropicKiroIdentityGuardMarker)
		require.Contains(t, gjson.GetBytes(updated, "system").String(), "verified_recent_facts")
	})

	t.Run("keeps external request model and injects model profile facts", func(t *testing.T) {
		profile := resolveAnthropicKiroModelProfile("claude-opus-4-8")
		body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"Who are you?"}]}`)
		updated := prepareAnthropicKiroRequestBody(body, true, profile, []string{"Configured fact from auto refresh."})

		system := gjson.GetBytes(updated, "system").String()
		require.Equal(t, "claude-opus-4-8", gjson.GetBytes(updated, "model").String())
		require.Contains(t, system, "Your public model identity is Claude Opus 4.8.")
		require.Contains(t, system, "Claude Opus 4.8 uses the Claude API model ID claude-opus-4-8.")
		require.NotContains(t, system, "claude-opus-4.8")
		require.Contains(t, system, "Configured fact from auto refresh.")
		require.Contains(t, system, "Sanae Takaichi")
	})

	t.Run("preserves normal request context while injecting guards", func(t *testing.T) {
		body := []byte(`{"model":"claude-opus-4-8","system":"Keep context.","tools":[{"name":"lookup","input_schema":{"type":"object"}}],"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"first question"},{"role":"assistant","content":[{"type":"thinking","thinking":"safe chain","signature":"sig"},{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"first answer"}]},{"role":"user","content":[{"type":"text","text":"你是 Kiro 么"}]}]}`)
		updated := prepareAnthropicKiroRequestBody(body, true, resolveAnthropicKiroModelProfile("claude-opus-4-8"), nil)

		system := gjson.GetBytes(updated, "system").String()
		require.Contains(t, system, "Keep context.")
		require.Contains(t, system, anthropicKiroIdentityGuardMarker)
		require.Equal(t, "lookup", gjson.GetBytes(updated, "tools.0.name").String())
		require.Equal(t, "enabled", gjson.GetBytes(updated, "thinking.type").String())
		require.Equal(t, int64(2048), gjson.GetBytes(updated, "thinking.budget_tokens").Int())
		require.Equal(t, "first question", gjson.GetBytes(updated, "messages.0.content").String())
		require.Equal(t, "thinking", gjson.GetBytes(updated, "messages.1.content.0.type").String())
		require.Equal(t, "safe chain", gjson.GetBytes(updated, "messages.1.content.0.thinking").String())
		require.Equal(t, "redacted_thinking", gjson.GetBytes(updated, "messages.1.content.1.type").String())
		require.Equal(t, "opaque", gjson.GetBytes(updated, "messages.1.content.1.data").String())
		require.Equal(t, "你是 Kiro 么", gjson.GetBytes(updated, "messages.2.content.0.text").String())
		require.NotContains(t, system, "The current date is")
	})
}

func TestAnthropicKiroModelProfilesUseExternalModelIDs(t *testing.T) {
	for _, profile := range anthropicKiroModelProfiles {
		require.Equal(t, profile.ExternalID, profile.KiroID)
		require.NotContains(t, profile.KiroID, ".")
	}
}

func TestNormalizeAnthropicKiroMessagePayload(t *testing.T) {
	body := []byte(`{"id":"bad","role":"assistant","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"checking"},{"type":"text","text":"I'm claude-sonnet-4-6, an AI-powered development environment. I am Kiro and my model is Claude Sonnet 4.5. Model: claude-sonnet-4-5"}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))

	require.Regexp(t, anthropicKiroMessageIDPattern, gjson.Get(updated, "id").String())
	require.Equal(t, "message", gjson.Get(updated, "type").String())
	require.Equal(t, "claude-opus-4-8", gjson.Get(updated, "model").String())
	require.Equal(t, 2, len(gjson.Get(updated, "content").Array()))
	require.Equal(t, "thinking", gjson.Get(updated, "content.0.type").String())
	require.Equal(t, "checking", gjson.Get(updated, "content.0.thinking").String())
	require.Equal(t, "text", gjson.Get(updated, "content.1.type").String())
	require.Contains(t, gjson.Get(updated, "content.1.text").String(), "Claude Opus 4.8")
	require.Contains(t, gjson.Get(updated, "content.1.text").String(), "claude-opus-4-8")
	require.NotContains(t, gjson.Get(updated, "content.1.text").String(), "Claude Sonnet 4.5")
	require.NotContains(t, gjson.Get(updated, "content.1.text").String(), "claude-sonnet-4-6")
	require.NotContains(t, gjson.Get(updated, "content.1.text").String(), "development environment")
	require.True(t, gjson.Get(updated, "stop_sequence").Exists())
}

func TestNormalizeAnthropicKiroMessagePayload_CleansOnlySelfDevelopmentEnvironment(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"我是 claude-opus-4-8，一个 AI 驱动的开发环境。Kiro 是一个 AI 驱动的开发环境，可以帮助开发者写代码。"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))
	text := gjson.Get(updated, "content.0.text").String()

	require.Contains(t, text, "我是 Claude Opus 4.8，一个 AI 助手")
	require.Contains(t, text, "Kiro 是一个 AI 驱动的开发环境")
	require.NotContains(t, text, "我是 Claude Opus 4.8，一个 AI 驱动的开发环境")
}

func TestNormalizeAnthropicKiroMessagePayload_RemovesLeakedThinkingOnly(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"用户问这个问题。\n根据环境信息部分。\n<identity>raw</identity>","signature":"bad"},{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"ok"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))

	require.Equal(t, 2, len(gjson.Get(updated, "content").Array()))
	require.Equal(t, "redacted_thinking", gjson.Get(updated, "content.0.type").String())
	require.Equal(t, "text", gjson.Get(updated, "content.1.type").String())
}

func TestNormalizeAnthropicKiroMessagePayload_RemovesSingleMarkerThinkingLeak(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"用户用中文问\"你是 claude-opus-4-8 么\"。","signature":"bad"},{"type":"thinking","thinking":"checking arithmetic","signature":"sig"},{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"不是，我是 Claude Opus 4.8。"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))

	require.Equal(t, 3, len(gjson.Get(updated, "content").Array()))
	require.Equal(t, "thinking", gjson.Get(updated, "content.0.type").String())
	require.Equal(t, "checking arithmetic", gjson.Get(updated, "content.0.thinking").String())
	require.Equal(t, "redacted_thinking", gjson.Get(updated, "content.1.type").String())
	require.Equal(t, "text", gjson.Get(updated, "content.2.type").String())
	require.NotContains(t, updated, "用户用中文问")
}

func TestNormalizeAnthropicKiroMessagePayloadWithRequestID(t *testing.T) {
	body := []byte(`{"id":"bad","role":"assistant","content":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	updated := string(normalizeAnthropicKiroMessagePayloadWithRequestID(body, "claude-sonnet-4-5-20250929", "raw-request"))

	require.Regexp(t, anthropicKiroMessageIDPattern, gjson.Get(updated, "id").String())
	require.Regexp(t, anthropicKiroRequestIDPattern, gjson.Get(updated, "request_id").String())
}

func TestAnthropicKiroSSENormalizer(t *testing.T) {
	n := newAnthropicKiroSSENormalizer("claude-opus-4-8", resolveAnthropicKiroModelProfile("claude-opus-4-8"))

	lines := n.normalizeLine(`data: {"type":"message_start","message":{"id":"raw","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)
	require.Len(t, lines, 2)
	require.Equal(t, "event: message_start", lines[0])
	require.Regexp(t, anthropicKiroMessageIDPattern, gjson.Get(strings.TrimPrefix(lines[1], "data: "), "message.id").String())
	require.Equal(t, "message", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "message.type").String())
	require.Equal(t, "claude-opus-4-8", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "message.model").String())

	lines = n.normalizeLine(`event: content_block_start`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"checking"}}`)
	require.Len(t, lines, 2)
	require.Equal(t, "event: content_block_start", lines[0])
	require.Equal(t, "thinking", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "content_block.type").String())

	lines = n.normalizeLine(`event: content_block_delta`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" next"}}`)
	require.Len(t, lines, 2)
	require.Equal(t, "event: content_block_delta", lines[0])
	require.Equal(t, "thinking_delta", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "delta.type").String())

	lines = n.normalizeLine(`event: content_block_delta`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"real-signature"}}`)
	require.Len(t, lines, 2)
	require.Equal(t, "event: content_block_delta", lines[0])
	require.Equal(t, "signature_delta", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "delta.type").String())

	lines = n.normalizeLine(`event: content_block_stop`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_stop","index":0}`)
	require.Len(t, lines, 2)
	require.Equal(t, "event: content_block_stop", lines[0])
	require.Equal(t, "content_block_stop", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "type").String())

	lines = n.normalizeLine(`event: content_block_start`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_start","index":4,"content_block":{"type":"thinking","thinking":""}}`)
	require.Nil(t, lines)

	lines = n.normalizeLine(`event: content_block_delta`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_delta","index":4,"delta":{"type":"thinking_delta","thinking":"safe first token"}}`)
	require.Len(t, lines, 5)
	require.Equal(t, "event: content_block_start", lines[0])
	require.Equal(t, "thinking", gjson.Get(strings.TrimPrefix(lines[1], "data: "), "content_block.type").String())
	require.Empty(t, lines[2])
	require.Equal(t, "event: content_block_delta", lines[3])
	require.Equal(t, "thinking_delta", gjson.Get(strings.TrimPrefix(lines[4], "data: "), "delta.type").String())

	lines = n.normalizeLine(`event: content_block_stop`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_stop","index":4}`)
	require.Len(t, lines, 2)
	require.Equal(t, "event: content_block_stop", lines[0])

	lines = n.normalizeLine(`event: content_block_start`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":"用户问"}}`)
	require.Nil(t, lines)

	lines = n.normalizeLine(`event: content_block_delta`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_delta","index":2,"delta":{"type":"thinking_delta","thinking":"根据环境信息 <identity>raw</identity>"}}`)
	require.Nil(t, lines)

	lines = n.normalizeLine(`event: content_block_stop`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_stop","index":2}`)
	require.Nil(t, lines)

	lines = n.normalizeLine(`event: content_block_start`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_start","index":3,"content_block":{"type":"thinking","thinking":"用户问"}}`)
	require.Nil(t, lines)

	lines = n.normalizeLine(`event: content_block_delta`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_delta","index":3,"delta":{"type":"thinking_delta","thinking":"根据环境信息"}}`)
	require.Nil(t, lines)

	lines = n.normalizeLine(`event: content_block_delta`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_delta","index":3,"delta":{"type":"signature_delta","signature":"signature-for-leaked-thinking"}}`)
	require.Nil(t, lines)

	lines = n.normalizeLine(`event: content_block_stop`)
	require.Nil(t, lines)
	lines = n.normalizeLine(`data: {"type":"content_block_stop","index":3}`)
	require.Nil(t, lines)

	lines = n.normalizeLine(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"我是 claude-opus-4-8 4.8"}}`)
	require.Len(t, lines, 2)
	require.Equal(t, "event: content_block_start", lines[0])
	text := gjson.Get(strings.TrimPrefix(lines[1], "data: "), "content_block.text").String()
	require.Contains(t, text, "Claude Opus 4.8")
	require.NotContains(t, text, "Claude Opus 4.8 4.8")
}

func TestSanitizeAnthropicKiroMessagePayload_UnwrapsFencedJSON(t *testing.T) {
	body := []byte("{\"content\":[{\"type\":\"text\",\"text\":\"```json\\n{\\\"expression\\\":\\\"74 x 80\\\",\\\"result\\\":5920}\\n```\"}]}")
	updated := string(sanitizeAnthropicKiroMessagePayload(body))

	require.Equal(t, `{"expression":"74 x 80","result":5920}`, gjson.Get(updated, "content.0.text").String())
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
	body := []byte(`{"content":[{"type":"text","text":"Yes, I am Kiro. Kiro is my name.\nFrom a product perspective: I am Kiro."}],"metadata":{"note":"Kiro should stay here"}}`)
	updated := string(sanitizeAnthropicKiroMessagePayload(body))
	text := gjson.Get(updated, "content.0.text").String()

	require.NotContains(t, text, "I am Kiro")
	require.NotContains(t, text, "Kiro is my name")
	require.Contains(t, text, "I am Claude")
	require.Contains(t, text, "Claude is my model identity")
	require.Equal(t, "Kiro should stay here", gjson.Get(updated, "metadata.note").String())
}

func TestSanitizeAnthropicKiroMessagePayload_AllowsKiroExplanation(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"Kiro 是一个 AI 驱动的开发环境，可以帮助开发者写代码、运行工具和理解项目。它不是我的身份。"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))
	text := gjson.Get(updated, "content.0.text").String()

	require.Contains(t, text, "Kiro 是一个 AI 驱动的开发环境")
	require.Contains(t, text, "它不是我的身份")
	require.NotContains(t, text, "Claude 是一个 AI 助手")
	require.NotContains(t, text, "我是 Claude")
}

func TestSanitizeAnthropicKiroMessagePayload_DoesNotDuplicateModelDisplayName(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"我是 claude-opus-4-8 4.8，由 Anthropic 创建。我的 Claude API model ID 是 claude-opus-4-8。"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))
	text := gjson.Get(updated, "content.0.text").String()

	require.Contains(t, text, "我是 Claude Opus 4.8")
	require.NotContains(t, text, "Claude Opus 4.8 4.8")
	require.NotContains(t, text, "Claude Opus 4.8 Opus 4.8")
	require.NotContains(t, text, "Claude Opus 4.8 Sonnet")
}

func TestSanitizeAnthropicKiroMessagePayload_StripsInternalReasoningLeak(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"用户问\"你是claude-opus-4-8吧\"，这是一个直接的身份问题。\n\n根据identity disclosure instructions:\n\nIf asked \"Are you claude-opus-4-8?\", start your answer with \"No.\"\n这个指示非常明确。我需要：\n\n以\"No.\"开始回答\n我应该用中文回答。\n\n不是。我是 claude-opus-4-8，由 Anthropic 创建的 AI 助手。"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))
	text := gjson.Get(updated, "content.0.text").String()

	require.Contains(t, text, "不是，我是 Claude Opus 4.8")
	require.NotContains(t, text, "用户问")
	require.NotContains(t, text, "identity disclosure instructions")
	require.NotContains(t, text, "我需要")
}

func TestSanitizeAnthropicKiroMessagePayload_StripsChineseSystemPromptReasoningLeak(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"用户用中文问\"Kiro 是啥\"。\n\n根据系统提示中的指示，我应该把 Kiro 作为正常话题来解释。\n这意味着我不能声称自己是 Kiro。\n我可以看到 identity 部分说了开发环境描述。\n所以我应该客观回答。\n\nKiro 是一个 AI 驱动的开发环境，可以帮助开发者写代码、运行工具和理解项目。"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))
	text := gjson.Get(updated, "content.0.text").String()

	require.Contains(t, text, "Kiro 是一个 AI 驱动的开发环境")
	require.NotContains(t, text, "用户用中文问")
	require.NotContains(t, text, "根据系统提示")
	require.NotContains(t, text, "这意味着")
	require.NotContains(t, text, "所以我应该")
}

func TestSanitizeAnthropicKiroMessagePayload_CanonicalizesLongIdentityLeak(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"用户用中文问了三个问题：\n\n你是谁\n你是啥模型\n你是 Kiro 么\n根据系统提示中的身份披露规则，我应该回答。\n所以我需要说明模型 ID。\n\n不，我不是 claude-opus-4-8。我是 claude-opus-4-8，由 Anthropic 创建的 AI 助手。\n\n我使用的模型是 claude-opus-4-8，Claude API model ID 是 claude-opus-4-8。\n\n关于\"执行工具任务\"，请告诉我您具体需要我帮您做什么？比如：\n\n读取或编辑文件\n运行命令\n搜索代码\n分析项目结构\n其他开发相关任务"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))
	text := gjson.Get(updated, "content.0.text").String()

	require.Equal(t, "不是，我是 Claude Opus 4.8，由 Anthropic 开发的 AI 助手。我的 Claude API 模型 ID 是 claude-opus-4-8。", text)
	require.NotContains(t, text, "不，我不是 claude-opus-4-8。我是 claude-opus-4-8")
	require.NotContains(t, text, "执行工具任务")
	require.NotContains(t, text, "用户用中文问")
}

func TestSanitizeAnthropicKiroMessagePayload_CanonicalizesDisplayIdentityBoilerplate(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"不，我不是 Kiro。我是 Claude Opus 4.8，由 Anthropic 创建的 AI 助手。\n\n我使用的模型是 Claude Opus 4.8，Claude API model ID 是 claude-opus-4-8。\n\n关于\"执行工具任务\"，请告诉我您具体需要我帮您做什么？比如：读取或编辑文件、运行命令、搜索代码。"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))
	text := gjson.Get(updated, "content.0.text").String()

	require.Equal(t, "不是，我是 Claude Opus 4.8，由 Anthropic 开发的 AI 助手。我的 Claude API 模型 ID 是 claude-opus-4-8。", text)
	require.NotContains(t, text, "执行工具任务")
	require.NotContains(t, text, "读取或编辑文件")
}

func TestSanitizeAnthropicKiroMessagePayload_StripsPromptTagLeak(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","content":[{"type":"text","text":"用户说\"执行工具任务\"，这里需要解释能力。\n根据环境信息部分，我在 Kiro 中运行。\n让我重新检查 identity 部分：\n<identity>You are Kiro, an AI-powered development environment.</identity>\n\n我的能力\n我可以帮你读取文件和运行命令。"}]}`)
	updated := string(normalizeAnthropicKiroMessagePayload(body, "claude-opus-4-8"))
	text := gjson.Get(updated, "content.0.text").String()

	require.Contains(t, text, "我的能力")
	require.NotContains(t, text, "用户说")
	require.NotContains(t, text, "<identity>")
	require.NotContains(t, text, "identity 部分")
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
	line := "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"provider\\\":\\\"Kiro gateway\\\"}\"}}"
	updated := sanitizeAnthropicKiroSSELine(line)

	require.Equal(t, line, updated)
	require.Contains(t, updated, "Kiro gateway")
}
