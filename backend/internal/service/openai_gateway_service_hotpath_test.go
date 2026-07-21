package service

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestExtractOpenAIRequestMetaFromBody(t *testing.T) {
	tests := []struct {
		name          string
		body          []byte
		wantModel     string
		wantStream    bool
		wantPromptKey string
	}{
		{
			name:          "完整字段",
			body:          []byte(`{"model":"gpt-5","stream":true,"prompt_cache_key":" ses-1 "}`),
			wantModel:     "gpt-5",
			wantStream:    true,
			wantPromptKey: "ses-1",
		},
		{
			name:          "缺失可选字段",
			body:          []byte(`{"model":"gpt-4"}`),
			wantModel:     "gpt-4",
			wantStream:    false,
			wantPromptKey: "",
		},
		{
			name:          "空请求体",
			body:          nil,
			wantModel:     "",
			wantStream:    false,
			wantPromptKey: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, stream, promptKey := extractOpenAIRequestMetaFromBody(tt.body)
			require.Equal(t, tt.wantModel, model)
			require.Equal(t, tt.wantStream, stream)
			require.Equal(t, tt.wantPromptKey, promptKey)
		})
	}
}

func TestExtractOpenAIReasoningEffortFromBody(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		model     string
		wantNil   bool
		wantValue string
	}{
		{
			name:      "优先读取 reasoning.effort",
			body:      []byte(`{"reasoning":{"effort":"medium"}}`),
			model:     "gpt-5-high",
			wantNil:   false,
			wantValue: "medium",
		},
		{
			name:      "兼容 reasoning_effort",
			body:      []byte(`{"reasoning_effort":"x-high"}`),
			model:     "",
			wantNil:   false,
			wantValue: "xhigh",
		},
		{
			name:      "GPT-5.6 显式 max 保留",
			body:      []byte(`{"reasoning":{"effort":"max"}}`),
			model:     "gpt-5.6-luna",
			wantNil:   false,
			wantValue: "max",
		},
		{
			name:      "非 GPT-5.6 显式 max 兼容为 xhigh",
			body:      []byte(`{"reasoning":{"effort":"max"}}`),
			model:     "gpt-5.4",
			wantNil:   false,
			wantValue: "xhigh",
		},
		{
			name:    "minimal 归一化为空",
			body:    []byte(`{"reasoning":{"effort":"minimal"}}`),
			model:   "gpt-5-high",
			wantNil: true,
		},
		{
			name:      "缺失字段时从模型后缀推导",
			body:      []byte(`{"input":"hi"}`),
			model:     "gpt-5-high",
			wantNil:   false,
			wantValue: "high",
		},
		{
			name:      "GPT-5.6 从 max 后缀推导",
			body:      []byte(`{"input":"hi"}`),
			model:     "gpt-5.6-luna-max",
			wantNil:   false,
			wantValue: "max",
		},
		{
			name:      "非 GPT-5.6 从 max 后缀推导为 xhigh",
			body:      []byte(`{"input":"hi"}`),
			model:     "gpt-5.4-max",
			wantNil:   false,
			wantValue: "xhigh",
		},
		{
			name:    "未知后缀不返回",
			body:    []byte(`{"input":"hi"}`),
			model:   "gpt-5-unknown",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractOpenAIReasoningEffortFromBody(tt.body, tt.model)
			if tt.wantNil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, tt.wantValue, *got)
		})
	}
}

func TestOpenAIUsageFromGJSON_ParsesCacheWriteAliases(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantCreation int
		wantRead     int
	}{
		{
			name:         "input details cache_write_tokens",
			body:         `{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":30,"cache_write_tokens":20}}}`,
			wantCreation: 20,
			wantRead:     30,
		},
		{
			name:         "prompt details cache_creation_tokens",
			body:         `{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":30,"cache_creation_tokens":21}}}`,
			wantCreation: 21,
			wantRead:     30,
		},
		{
			name:         "top-level cache_creation_input_tokens",
			body:         `{"usage":{"input_tokens":100,"output_tokens":5,"cache_creation_input_tokens":22}}`,
			wantCreation: 22,
		},
		{
			name:         "nested cache_creation_input_tokens",
			body:         `{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cache_creation_input_tokens":23}}}`,
			wantCreation: 23,
		},
		{
			name:         "zero nested aliases do not hide top-level values",
			body:         `{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":0,"cache_creation_input_tokens":0},"cache_read_input_tokens":31,"cache_creation_input_tokens":24}}`,
			wantCreation: 24,
			wantRead:     31,
		},
		{
			name: "all cache aliases zero",
			body: `{"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":0,"cache_creation_input_tokens":0},"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage, ok := openAIUsageFromGJSON(gjson.Get(tt.body, "usage"))
			require.True(t, ok)
			require.Equal(t, tt.wantCreation, usage.CacheCreationInputTokens)
			require.Equal(t, tt.wantRead, usage.CacheReadInputTokens)
		})
	}
}

func TestReconstructResponseOutputFromSSE_IncludesReasoningTextDelta(t *testing.T) {
	output, ok := reconstructResponseOutputFromSSE("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"private plan\"}\n\n")
	require.True(t, ok)
	require.Equal(t, "reasoning", gjson.GetBytes(output, "0.type").String())
	require.Equal(t, "private plan", gjson.GetBytes(output, "0.summary.0.text").String())
}

func TestReconstructResponseOutputFromSSE_IncludesCustomToolCallInputDelta(t *testing.T) {
	output, ok := reconstructResponseOutputFromSSE("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"call_1\",\"name\":\"shell\"}}\n\n" +
		"data: {\"type\":\"response.custom_tool_call_input.delta\",\"output_index\":0,\"delta\":\"echo hi\"}\n\n")
	require.True(t, ok)
	require.Equal(t, "function_call", gjson.GetBytes(output, "0.type").String())
	require.Equal(t, "call_1", gjson.GetBytes(output, "0.call_id").String())
	require.Equal(t, "shell", gjson.GetBytes(output, "0.name").String())
	require.Equal(t, "echo hi", gjson.GetBytes(output, "0.arguments").String())
}

func TestGetOpenAIRequestBodyMap_UsesContextCache(t *testing.T) {
	setGinTestMode()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	cached := map[string]any{"model": "cached-model", "stream": true}
	c.Set(OpenAIParsedRequestBodyKey, cached)

	got, err := getOpenAIRequestBodyMap(c, []byte(`{invalid-json`))
	require.NoError(t, err)
	require.Equal(t, cached, got)
}

func TestDeriveOpenAIVirtualPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","instructions":"be helpful","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`)
	accountA := &Account{
		ID:       1,
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"prompt_cache_boost_enabled": true,
		},
	}
	accountB := &Account{
		ID:       2,
		Type:     AccountTypeAPIKey,
		Platform: PlatformOpenAI,
		Credentials: map[string]any{
			"pool_mode":                  true,
			"prompt_cache_boost_enabled": true,
		},
	}

	key1 := deriveOpenAIVirtualPromptCacheKey(accountA, "gpt-5.5", body)
	key2 := deriveOpenAIVirtualPromptCacheKey(accountA, "gpt-5.5", body)
	keyOtherAccount := deriveOpenAIVirtualPromptCacheKey(accountB, "gpt-5.5", body)
	keyOtherModel := deriveOpenAIVirtualPromptCacheKey(accountA, "gpt-5.4", body)

	require.NotEmpty(t, key1)
	require.Equal(t, key1, key2)
	require.NotEqual(t, key1, keyOtherAccount)
	require.NotEqual(t, key1, keyOtherModel)
}

func TestGetOpenAIRequestBodyMap_ParseErrorWithoutCache(t *testing.T) {
	_, err := getOpenAIRequestBodyMap(nil, []byte(`{invalid-json`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse request")
}

func TestGetOpenAIRequestBodyMap_WriteBackContextCache(t *testing.T) {
	setGinTestMode()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	got, err := getOpenAIRequestBodyMap(c, []byte(`{"model":"gpt-5","stream":true}`))
	require.NoError(t, err)
	require.Equal(t, "gpt-5", got["model"])

	cached, ok := c.Get(OpenAIParsedRequestBodyKey)
	require.True(t, ok)
	cachedMap, ok := cached.(map[string]any)
	require.True(t, ok)
	require.Equal(t, got, cachedMap)
}

func TestSanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(t *testing.T) {
	var reqBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{
		"model":"gpt-5.4",
		"input":[
			{"role":"user","content":[
				{"type":"input_text","text":"Describe this"},
				{"type":"input_image","image_url":"data:image/png;base64,   "},
				{"type":"input_image","image_url":"data:image/png;base64,abc123"}
			]},
			{"role":"user","content":[
				{"type":"input_image","image_url":"data:image/png;base64,"}
			]},
			{"type":"input_image","image_url":"data:image/png;base64,"},
			{"type":"input_image","image_url":"data:image/png;base64,top-level-valid"}
		]
	}`), &reqBody))

	require.True(t, sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody))

	normalized, err := json.Marshal(reqBody)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"model":"gpt-5.4",
		"input":[
			{"role":"user","content":[
				{"type":"input_text","text":"Describe this"},
				{"type":"input_image","image_url":"data:image/png;base64,abc123"}
			]},
			{"type":"input_image","image_url":"data:image/png;base64,top-level-valid"}
		]
	}`, string(normalized))
}

func TestSanitizeEmptyBase64InputImagesInOpenAIBody(t *testing.T) {
	body, changed, err := sanitizeEmptyBase64InputImagesInOpenAIBody([]byte(`{
		"model":"gpt-5.4",
		"stream":true,
		"input":[
			{"role":"user","content":[
				{"type":"input_text","text":"Describe this"},
				{"type":"input_image","image_url":"data:image/png;base64,"}
			]}
		]
	}`))
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{
		"model":"gpt-5.4",
		"stream":true,
		"input":[
			{"role":"user","content":[
				{"type":"input_text","text":"Describe this"}
			]}
		]
	}`, string(body))
}
