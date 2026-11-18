package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestV0223ShortTerminalAndAuthoritativeEnvelope(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, tc := range []struct {
		body          string
		input, output int
		valid         bool
	}{
		{`{"type":"response.done","usage":{"input_tokens":1,"output_tokens":1}}`, 1, 1, true},
		{`{"type":"response.completed","response":{"usage":{"input_tokens":1}}}`, 1, 0, true},
		{`{"type":"response.completed","usage":{},"response":{"usage":{"input_tokens":123,"output_tokens":45}}}`, 123, 45, true},
		{`{"type":"response.failed","usage":{"input_tokens":999},"response":{"usage":{"input_tokens":0,"prompt_tokens":999,"output_tokens":2}}}`, 0, 2, true},
		{`{"type":"response.done","usage":{"input_tokens":3},"response":{"usage":{}}}`, 3, 0, true},
		{`{"type":"response.output_text.delta","usage":{"input_tokens":8}}`, 0, 0, false},
		{`[DONE]`, 0, 0, false}, {`{"type":`, 0, 0, false},
	} {
		var usage OpenAIUsage
		svc.parseSSEUsageBytes([]byte(tc.body), &usage)
		require.Equal(t, tc.input, usage.InputTokens, tc.body)
		require.Equal(t, tc.output, usage.OutputTokens, tc.body)
	}
	for _, raw := range []string{`-1`, `1.5`, `9223372036854775808`, `true`, `[]`} {
		u, ok := extractOpenAIUsageFromJSONBytes([]byte(`{"usage":{"input_tokens":` + raw + `,"output_tokens":4}}`))
		require.True(t, ok)
		require.True(t, u.usageInvalid, raw)
		require.Equal(t, 4, u.OutputTokens)
	}
	u, ok := extractOpenAIUsageFromJSONBytes([]byte(`{"usage":{"input_tokens":0,"prompt_tokens":9,"cache_read_input_tokens":0,"cached_tokens":6}}`))
	require.True(t, ok)
	require.Zero(t, u.InputTokens)
	require.Zero(t, u.CacheReadInputTokens)
}

func TestV0223AnthropicPartialTTLAndPromptTotal(t *testing.T) {
	usage := &ClaudeUsage{}
	parseSSEUsagePassthrough(`{"type":"message_start","message":{"usage":{"prompt_tokens":100,"input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":20},"cached_tokens":30}}}`, usage)
	require.Equal(t, 50, usage.InputTokens)
	require.Equal(t, 20, usage.CacheCreationInputTokens)
	parseSSEUsagePassthrough(`{"type":"message_delta","usage":{"cache_creation":{"ephemeral_1h_input_tokens":10}}}`, usage)
	require.Equal(t, 40, usage.InputTokens)
	require.Equal(t, 30, usage.CacheCreationInputTokens)
	parseSSEUsagePassthrough(`{"type":"message_delta","usage":{"cache_creation":{"ephemeral_5m_input_tokens":0},"output_tokens":2}}`, usage)
	require.Equal(t, 10, usage.CacheCreationInputTokens)
	require.Equal(t, 60, usage.InputTokens)
	native := claudeUsageToOpenAIUsage(usage)
	require.Equal(t, 100, native.InputTokens)
	require.Equal(t, 10, native.CacheCreation1hTokens)
	// Positive authoritative aggregate survives zero placeholder deltas and partial details.
	usage = parseClaudeUsageFromResponseBody([]byte(`{"usage":{"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":30}}}`))
	parseSSEUsagePassthrough(`{"type":"message_delta","usage":{"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_1h_input_tokens":40}}}`, usage)
	require.Equal(t, 100, usage.CacheCreationInputTokens)
	require.True(t, applyCacheTTLOverride(usage, "1h"))
	require.Equal(t, 100, usage.CacheCreation1hTokens)
}

func TestV0223CacheCreationBillingMatchesStats(t *testing.T) {
	for _, tc := range []struct{ total, a, b, wantA, wantB int }{
		{100, 20, 30, 70, 30}, {100, 0, 0, 100, 0}, {0, 20, 30, 20, 30}, {100, 120, 80, 60, 40},
	} {
		tokens := UsageTokens{CacheCreationTokens: tc.total, CacheCreation5mTokens: tc.a, CacheCreation1hTokens: tc.b}
		a, b := normalizeCacheCreationBreakdown(tokens)
		require.Equal(t, tc.wantA, a)
		require.Equal(t, tc.wantB, b)
		p5, p1 := 1.0, 2.0
		cost := (&BillingService{}).computeCacheCreationCost(&ModelPricing{SupportsCacheBreakdown: true, CacheCreation5mPrice: p5, CacheCreation1hPrice: p1}, tokens, p5, 1)
		stats := calculateTokenStatsCost(&ChannelModelPricing{CacheWritePrice: &p5, CacheWrite1hPrice: &p1}, tokens)
		require.NotNil(t, stats)
		require.Equal(t, cost, *stats)
	}
	maxInt := int(^uint(0) >> 1)
	usage := claudeUsageToOpenAIUsage(&ClaudeUsage{InputTokens: maxInt, CacheReadInputTokens: 1})
	require.True(t, usage.usageInvalid)
}

func TestV0223TTLOverrideClearsConflictingBucket(t *testing.T) {
	const raw = `{"usage":{"cache_creation_input_tokens":100,"cache_creation":{"ephemeral_1h_input_tokens":100,"ephemeral_5m_input_tokens":10}}}`
	usage := parseClaudeUsageFromResponseBody([]byte(raw))
	require.True(t, applyCacheTTLOverride(usage, "1h"))
	require.Zero(t, usage.CacheCreation5mTokens)
	require.Equal(t, 100, usage.CacheCreation1hTokens)
	var body struct {
		Usage map[string]any `json:"usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &body))
	require.True(t, rewriteCacheCreationJSON(body.Usage, "1h"))
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	wire := gjson.GetBytes(encoded, "usage.cache_creation")
	require.EqualValues(t, 0, wire.Get("ephemeral_5m_input_tokens").Int())
	require.EqualValues(t, 100, wire.Get("ephemeral_1h_input_tokens").Int())
}

func TestV0223TypedUsageMatchesRawUsage(t *testing.T) {
	raw := []byte(`{"usage":{"input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":7,"ephemeral_1h_input_tokens":13},"output_tokens":2}}`)
	want := parseClaudeUsageFromResponseBody(raw)
	got := &ClaudeUsage{}
	a, b := 7, 13
	mergeAnthropicUsage(got, apicompat.AnthropicUsage{CacheCreation: &apicompat.AnthropicCacheCreation{Ephemeral5mInputTokens: &a, Ephemeral1hInputTokens: &b}, OutputTokens: 2})
	require.Equal(t, want.CacheCreationInputTokens, got.CacheCreationInputTokens)
	require.Equal(t, want.CacheCreation1hTokens, got.CacheCreation1hTokens)
	// A hit-only alias identifies the cached bucket without turning input_tokens into a total.
	var typed apicompat.AnthropicUsage
	require.NoError(t, json.Unmarshal([]byte(`{"input_tokens":40,"prompt_cache_hit_tokens":60}`), &typed))
	got = &ClaudeUsage{}
	mergeAnthropicUsage(got, typed)
	require.Equal(t, parseClaudeUsageFromResponseBody([]byte(`{"usage":{"input_tokens":40,"prompt_cache_hit_tokens":60}}`)), got)
	media := parseClaudeUsageFromResponseBody([]byte(`{"usage":{"input_tokens":1,"image_output_tokens":9}}`))
	require.Equal(t, 9, media.ImageOutputTokens, "retain existing non-stream media usage")
}

func TestV0223AnthropicChatNormalizesUsageWithoutLosingTerminal(t *testing.T) {
	setGinTestMode()
	for _, tc := range []struct {
		name, ttl string
		invalid   bool
	}{
		{name: "exact decimal and string counts", ttl: "1e1"},
		{name: "invalid TTL count", ttl: "1.5", invalid: true},
	} {
		for _, stream := range []bool{true, false} {
			name := tc.name + "/buffered"
			if stream {
				name = tc.name + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				body := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_fixture","content":[],"usage":{"prompt_tokens":1e2,"cached_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20}}}}` + "\n\n" +
					"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}` + "\n\n" +
					"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":"2","cache_creation":{"ephemeral_1h_input_tokens":` + tc.ttl + `}}}` + "\n\n" +
					"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
				svc := &GatewayService{}
				var result *ForwardResult
				var err error
				var clientUsage gjson.Result
				if stream {
					result, err = svc.handleCCStreamingFromAnthropic(resp, c, "public", "provider", nil, time.Now(), true)
					require.Equal(t, 1, strings.Count(rec.Body.String(), "data: [DONE]"))
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						usage := gjson.Get(strings.TrimPrefix(line, "data: "), "usage")
						if usage.IsObject() {
							clientUsage = usage
						}
					}
				} else {
					result, err = svc.handleCCBufferedFromAnthropic(resp, c, "public", "provider", nil, time.Now())
					clientUsage = gjson.Get(rec.Body.String(), "usage")
				}
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), `"finish_reason":"length"`)
				require.Contains(t, rec.Body.String(), "hello")
				require.Equal(t, 2, result.Usage.OutputTokens)
				require.Equal(t, 30, result.Usage.CacheReadInputTokens)
				require.Equal(t, 20, result.Usage.CacheCreation5mTokens)
				require.Equal(t, tc.invalid, result.Usage.usageInvalid)
				if tc.invalid {
					require.False(t, clientUsage.IsObject())
					return
				}
				require.Equal(t, 40, result.Usage.InputTokens)
				require.Equal(t, 10, result.Usage.CacheCreation1hTokens)
				require.EqualValues(t, 100, clientUsage.Get("prompt_tokens").Int())
				require.EqualValues(t, 30, clientUsage.Get("prompt_tokens_details.cached_tokens").Int())
				require.EqualValues(t, 30, clientUsage.Get("prompt_tokens_details.cache_creation_tokens").Int())
				require.EqualValues(t, 102, clientUsage.Get("total_tokens").Int())
			})
		}
	}
}

func TestV0223MapAndRawCountsAgreeExactly(t *testing.T) {
	for _, raw := range []string{
		`{"input_tokens":9007199254740993,"output_tokens":2}`,
		`{"input_tokens":0,"cached_tokens":9,"cache_read_input_tokens":0}`,
		`{"cache_creation":{"ephemeral_1h_input_tokens":15}}`,
		`{"input_tokens":1.5,"output_tokens":2}`,
	} {
		var node map[string]any
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		require.NoError(t, decoder.Decode(&node))
		require.Equal(t, anthropicUsagePatchFromNode(gjson.Parse(raw), true), anthropicUsagePatchFromMap(node, true), raw)
	}
}

func TestV0223NativeTTLIsRecordedAndBilled(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	userRepo := &openAIRecordUsageUserRepoStub{}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
	upstream := &ClaudeUsage{InputTokens: 10, OutputTokens: 2, CacheReadInputTokens: 30, CacheCreationInputTokens: 100, CacheCreation5mTokens: 20, CacheCreation1hTokens: 30}
	result := &OpenAIForwardResult{RequestID: "v0223-ttl", Model: "claude-sonnet-5-5", Usage: claudeUsageToOpenAIUsage(upstream)}
	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: result, APIKey: &APIKey{ID: 1, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 2}, Account: &Account{ID: 3, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
	})
	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, 10, usageRepo.lastLog.InputTokens)
	require.Equal(t, 100, usageRepo.lastLog.CacheCreationTokens)
	require.Equal(t, 20, usageRepo.lastLog.CacheCreation5mTokens)
	require.Equal(t, 30, usageRepo.lastLog.CacheCreation1hTokens)
	price, err := svc.billingService.GetModelPricing(result.Model)
	require.NoError(t, err)
	require.InDelta(t, 70*price.CacheCreation5mPrice+30*price.CacheCreation1hPrice, usageRepo.lastLog.CacheCreationCost, 1e-12)
	wire, err := json.Marshal(result.Usage)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "cache_creation_5m_tokens")
	require.NotContains(t, string(wire), "cache_creation_1h_tokens")
	result.Usage.usageInvalid = true
	calls := usageRepo.calls
	require.Error(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: result}))
	require.Equal(t, calls, usageRepo.calls, "invalid counts must not produce a successful zero-cost bill")
	result.Usage = OpenAIUsage{InputTokens: int(^uint(0) >> 1), OutputTokens: 1}
	require.Error(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: result}))
	require.Equal(t, calls, usageRepo.calls, "aggregate overflow must be rejected before success side effects")
}

func TestV0223NativeBufferedCacheOnlyPartialTTL(t *testing.T) {
	body := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{"prompt_tokens":100,"cached_tokens":30,"cache_creation":{"ephemeral_5m_input_tokens":20}}}}` + "\n\n" +
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"cache_creation":{"ephemeral_1h_input_tokens":10},"output_tokens":2}}` + "\n\n" +
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	response, usage, err := (&OpenAIGatewayService{}).readNativeAnthropicResponse(&http.Response{Body: io.NopCloser(strings.NewReader(body))})
	require.NoError(t, err)
	require.Equal(t, 40, usage.InputTokens)
	require.Equal(t, 30, usage.CacheCreationInputTokens)
	require.Equal(t, 10, usage.CacheCreation1hTokens)
	converted := apicompat.AnthropicToResponsesResponse(response)
	require.Equal(t, 100, converted.Usage.InputTokens)
	require.Equal(t, 30, converted.Usage.CacheCreationInputTokens)
	require.Equal(t, 2, converted.Usage.OutputTokens)
}
