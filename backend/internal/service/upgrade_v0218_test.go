//go:build unit

package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUpgradeGrokFastPricingBoundary(t *testing.T) {
	svc := newTestBillingService()
	for _, model := range []string{"grok-4.7-fast", "grok-4.7-build-fast"} {
		for _, input := range []int{199999, 200000, 200001} {
			cost, err := svc.CalculateCost(model, UsageTokens{InputTokens: input - 100, CacheReadTokens: 100, OutputTokens: 100}, 1)
			require.NoError(t, err)
			factor := 1.0
			if input >= 200000 {
				factor = 1.5
			}
			require.InDelta(t, float64(input-100)*4e-6*factor+100*1e-6*factor+100*12e-6*factor, cost.TotalCost, 1e-10)
		}
	}
}

func TestUpgradeSchemaLiteralNamesAndConstants(t *testing.T) {
	input := map[string]any{"type": "object", "properties": map[string]any{
		"examples": map[string]any{"type": "string", "const": "yes"},
		"const":    map[string]any{"type": "integer", "exclusiveMinimum": json.Number("4")},
		"enum":     map[string]any{"type": "string", "const": "x", "enum": []any{"y"}},
		"default":  map[string]any{"type": "object", "default": map[string]any{"type": "literal", "examples": []any{1}, "const": 42}},
	}, "examples": []any{1}, "const": 42}
	cleaned := cleanToolSchema(input).(map[string]any)
	props := cleaned["properties"].(map[string]any)
	require.Equal(t, []any{"yes"}, props["examples"].(map[string]any)["enum"])
	require.Equal(t, json.Number("5"), props["const"].(map[string]any)["minimum"])
	require.Empty(t, props["enum"].(map[string]any)["enum"])
	require.Equal(t, map[string]any{"type": "literal", "examples": []any{1}, "const": 42}, props["default"].(map[string]any)["default"])
	require.NotContains(t, cleaned, "examples")
	require.NotContains(t, cleaned, "const")
	require.Equal(t, 42, input["const"], "the source schema is immutable")
}

func TestUpgradeGeminiNegativeOrdinaryUsage(t *testing.T) {
	for _, prompt := range []int{0, 3, 10, 20} {
		usage := extractGeminiUsage([]byte(fmt.Sprintf(`{"usageMetadata":{"promptTokenCount":%d,"cachedContentTokenCount":10,"candidatesTokenCount":2}}`, prompt)))
		expected := prompt - 10
		if expected < 0 {
			expected = 0
		}
		require.Equal(t, expected, usage.InputTokens)
		require.Equal(t, 10, usage.CacheReadInputTokens)
	}
}

func TestUpgradeAnthropicCompatTTFTIgnoresNonOutputFrames(t *testing.T) {
	contentStart := &apicompat.AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &apicompat.AnthropicContentBlock{Type: "text"}}
	cases := []struct {
		name  string
		event *apicompat.AnthropicStreamEvent
		want  bool
	}{
		{"ping", &apicompat.AnthropicStreamEvent{Type: "ping"}, false},
		{"empty text keepalive", &apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "text_delta"}}, false},
		{"signature only", &apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "signature_delta", Signature: "sig"}}, false},
		{"empty content start", contentStart, false},
		{"text", &apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "text_delta", Text: "hello"}}, true},
		{"thinking", &apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "thinking_delta", Thinking: "reasoning"}}, true},
		{"tool arguments", &apicompat.AnthropicStreamEvent{Type: "content_block_delta", Delta: &apicompat.AnthropicDelta{Type: "input_json_delta", PartialJSON: "{"}}, true},
		{"tool start", &apicompat.AnthropicStreamEvent{Type: "content_block_start", ContentBlock: &apicompat.AnthropicContentBlock{Type: "tool_use"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, anthropicCompatStartsRealOutput(tc.event))
		})
	}
}

func TestUpgradeDeepSeekImageCanonicalization(t *testing.T) {
	image := map[string]any{"type": "image_url", "url": "legacy", "image_url": map[string]any{"url": "canonical"}}
	tool := map[string]any{"arguments": map[string]any{"type": "image_url", "url": "literal"}}
	body := map[string]any{"content": []any{image, tool}}
	aliasDeepSeekResponsesImages(body)
	require.Equal(t, "input_image", image["type"])
	require.Equal(t, "canonical", image["image_url"])
	require.NotContains(t, image, "url")
	require.Equal(t, "literal", tool["arguments"].(map[string]any)["url"])
}

func TestUpgradeCloudflare1010Evidence(t *testing.T) {
	for _, body := range []string{
		`{"cloudflare_error":true,"error_code":1010}`, `{"errors":[{"error_name":"browser_signature_banned","code":"1010"}]}`,
		`{"cloudflare_error":true,"error":{"code":1010}}`,
	} {
		require.True(t, isCloudflare1010JSON([]byte(body)), body)
	}
	for _, body := range []string{`{"code":1010}`, `{"error":{"code":"invalid_api_key"}}`, `{"cloudflare_error":false,"code":1010}`, `{"cloudflare_error":true,"code":401}`, `not json`} {
		require.False(t, isCloudflare1010JSON([]byte(body)), body)
	}
}

func TestUpgradeQuotaCapabilityIsolation(t *testing.T) {
	for _, platform := range []string{PlatformGemini, PlatformAnthropic, PlatformOpenAI, PlatformAntigravity} {
		a := &Account{Platform: platform, Type: AccountTypeServiceAccount}
		require.Equal(t, platform == PlatformGemini, a.SupportsAccountQuota())
		require.False(t, a.IsAPIKeyOrBedrock())
	}
}

func TestUpgradeVertexQuotaSchedulingAndBillingRemainSeparate(t *testing.T) {
	for _, platform := range []string{PlatformGemini, PlatformAnthropic} {
		a := &Account{ID: 7, Platform: platform, Type: AccountTypeServiceAccount, Status: StatusActive, Schedulable: true,
			Extra: map[string]any{"quota_limit": 10.0, "quota_used": 10.0}}
		require.Equal(t, platform != PlatformGemini, a.IsSchedulable())
		p := &postUsageBillingParams{Account: a, APIKey: &APIKey{ID: 2}, User: &User{ID: 3},
			Cost: &CostBreakdown{TotalCost: 3, ActualCost: 6}, AccountRateMultiplier: 0.2}
		usage := &UsageLog{InputTokens: 12, CacheReadTokens: 7}
		cmd := buildUsageBillingCommand("vertex-quota", usage, p)
		require.Equal(t, platform, cmd.AccountPlatform)
		require.Equal(t, AccountTypeServiceAccount, cmd.AccountType)
		require.Equal(t, 6.0, cmd.BalanceCost)
		if platform == PlatformGemini {
			require.InDelta(t, 0.6, cmd.AccountQuotaCost, 1e-12)
		} else {
			require.Zero(t, cmd.AccountQuotaCost)
		}
		require.Equal(t, 12, cmd.InputTokens)
		require.Equal(t, 7, cmd.CacheReadTokens)
		require.False(t, a.IsAPIKeyOrBedrock(), "quota support must not enable pool features")
	}
}

func TestUpgradeLazyRetryProbe(t *testing.T) {
	calls := 0
	ctx := WithSingleAccountRetryProbe(context.Background(), func(context.Context) bool { calls++; return true })
	require.Zero(t, calls)
	for i := 0; i < 3; i++ {
		v, ok := SingleAccountRetryFromContext(ctx)
		require.True(t, ok)
		require.True(t, v)
	}
	require.Equal(t, 1, calls)
	explicit := WithSingleAccountRetry(ctx, false, true)
	v, ok := SingleAccountRetryFromContext(explicit)
	require.True(t, ok)
	require.False(t, v)
	ctx, cancel := context.WithCancel(WithSingleAccountRetryProbe(context.Background(), func(context.Context) bool { t.Fatal("cancelled probe queried"); return false }))
	cancel()
	v, _ = SingleAccountRetryFromContext(ctx)
	require.False(t, v)
}

func TestUpgradeSignatureOrderMutationAndAssociation(t *testing.T) {
	a := map[string]any{"a": "one", "b": "two"}
	b := map[string]any{"b": "two", "a": "one"}
	old := modelMappingSignature(a)
	require.Equal(t, old, modelMappingSignature(b))
	a["a"] = "two"
	a["b"] = "one"
	require.NotEqual(t, old, modelMappingSignature(a))
	a["a"] = true
	require.NotEqual(t, old, modelMappingSignature(a))
	require.NotEqual(t, modelMappingSignature(map[string]any{"a": ""}), modelMappingSignature(map[string]any{"a": nil}))
	require.Zero(t, modelMappingSignature(nil))
}

func TestUpgradeSafeCompatFailureTerminals(t *testing.T) {
	setGinTestMode()
	for _, responses := range []bool{false, true} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/", nil)
		state := apicompat.NewAnthropicEventToResponsesState()
		state.ResponseID = "msg_safe"
		state.SequenceNumber = 9
		writeSafeGatewayStreamFailure(c, responses, state)
		writeSafeGatewayStreamFailure(c, responses, state)
		output := rec.Body.String()
		require.Equal(t, 1, strings.Count(output, "Upstream request failed"))
		require.True(t, strings.HasSuffix(output, "\n\n"))
		if responses {
			require.Contains(t, output, "event: response.failed\n")
			require.Contains(t, output, `"id":"msg_safe"`)
			require.Contains(t, output, `"sequence_number":9`)
			require.Contains(t, output, `"output":[]`)
		} else {
			require.Contains(t, output, "data: [DONE]\n\n")
		}
	}
}

type upgradeTailReader struct{ done bool }

func (r *upgradeTailReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, `data: {"usageMetadata":{"promptTokenCount":999},"secret":"tail.invalid"}`), errors.New("truncated")
}
func TestUpgradeGeminiNonEOFTailNotCounted(t *testing.T) {
	setGinTestMode()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/", nil)
	svc := &GeminiMessagesCompatService{cfg: &config.Config{}}
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&upgradeTailReader{})}
	result, _ := svc.handleNativeStreamingResponse(c, resp, time.Now(), false)
	require.NotNil(t, result)
	require.Zero(t, result.usage.InputTokens)
	require.NotContains(t, rec.Body.String(), "tail.invalid")
}

func TestUpgradeDisconnectDrainHasAbsoluteBound(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	pump := newAnthropicNativeLinePump(bufio.NewScanner(reader), 0)
	defer pump.stop()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(30*time.Millisecond))
	defer cancel()
	marked := false
	pump.watchClient(ctx, func() { marked = true })
	pump.beginDrain()
	_, err := pump.next()
	require.ErrorIs(t, err, errAnthropicCompatDrainTimeout)
	require.True(t, marked)
}

func legacyMappingSignature(mapping map[string]any) uint64 {
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := fnv.New64a()
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		if s, ok := mapping[k].(string); ok {
			_, _ = h.Write([]byte(s))
		} else {
			_, _ = h.Write([]byte{1})
		}
		_, _ = h.Write([]byte{255})
	}
	return h.Sum64()
}
func BenchmarkUpgradeMappingSignature(b *testing.B) {
	for _, size := range []int{1, 8, 32, 128} {
		mapping := map[string]any{}
		for i := 0; i < size; i++ {
			mapping[fmt.Sprintf("model-%d", i)] = fmt.Sprintf("target-%d", i)
		}
		for _, fast := range []bool{false, true} {
			b.Run(fmt.Sprintf("size=%d/fast=%v", size, fast), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if fast {
						modelMappingSignature(mapping)
					} else {
						legacyMappingSignature(mapping)
					}
				}
			})
		}
	}
}

func TestUpgradeGeminiImageGateScopeModalitiesAndBodyRevision(t *testing.T) {
	setGinTestMode()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	denied := &Group{Platform: PlatformGemini}
	allowed := &Group{Platform: PlatformGemini, AllowImageGeneration: true}
	for _, body := range []string{
		`{"generationConfig":{"responseModalities":["TEXT","IMAGE"]}}`,
		`{"generation_config":{"response_modalities":[2]}}`,
		`{"generationConfig":{"responseModalities":["TEXT"],"responseModalities":["IMAGE"]}}`,
		`{"generationConfig":{},"generationConfig":{"responseModalities":["IMAGE"]}}`,
	} {
		require.True(t, GeminiImageGenerationDeniedForContext(c, denied, []byte(body), "gemini-text"), body)
		require.False(t, GeminiImageGenerationDeniedForContext(c, allowed, []byte(body)), body)
		require.False(t, GeminiImageGenerationDenied(&Group{Platform: PlatformOpenAI}, []byte(body)), body)
	}
	require.True(t, GeminiImageGenerationDenied(denied, nil, "models/gemini-3-pro-image"))
	require.False(t, GeminiImageGenerationDenied(nil, nil, "gemini-3-pro-image"))
	body := []byte(`{"generationConfig":{"responseModalities":["IMAGE"]}}`)
	require.True(t, GeminiImageGenerationDeniedForContext(c, denied, body))
	copy(body, []byte(`{"generationConfig":{"responseModalities":["AUDIO"]}}`))
	require.False(t, GeminiImageGenerationDeniedForContext(c, denied, body), "in-place body rewrites invalidate request facts")
}

func TestUpgradeHeaderScanEquivalentIncludingUnicodeLegacyKeys(t *testing.T) {
	overrides := map[string]string{"x-test": "new", "x-key": "value", "user-agent": "agent", "x-long": "long"}
	initial := http.Header{"X-TEST": {"old"}, "x-TeSt": {"duplicate"}, "X-Key": {"legacy unicode"}, "User-Agent": {"old agent"}, "Untouched": {"keep"}, strings.Repeat("x", 300): {"long"}}
	want, got := initial.Clone(), initial.Clone()
	legacyHeaderOverrides(want, overrides)
	applyHeaderOverrideEntries(got, overrides)
	require.Equal(t, want, got)
}

func legacyHeaderOverrides(h http.Header, overrides map[string]string) {
	for name, value := range overrides {
		for existing := range h {
			if strings.EqualFold(existing, name) {
				delete(h, existing)
			}
		}
		h[resolveWireCasing(name)] = []string{value}
	}
}
func BenchmarkUpgradeHeaderOverrides(b *testing.B) {
	for _, size := range []int{1, 4, 16} {
		overrides := map[string]string{}
		initial := http.Header{}
		for i := 0; i < 32; i++ {
			initial[fmt.Sprintf("X-Header-%d", i)] = []string{"old"}
		}
		for i := 0; i < size; i++ {
			overrides[fmt.Sprintf("x-header-%d", i)] = "new"
		}
		for _, fast := range []bool{false, true} {
			b.Run(fmt.Sprintf("size=%d/fast=%v", size, fast), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					h := initial.Clone()
					if fast {
						applyHeaderOverrideEntries(h, overrides)
					} else {
						legacyHeaderOverrides(h, overrides)
					}
				}
			})
		}
	}
}

type upgradeFailingWriter struct {
	gin.ResponseWriter
	attempts int
}

func (w *upgradeFailingWriter) Write(p []byte) (int, error)       { w.attempts++; return 0, io.ErrClosedPipe }
func (w *upgradeFailingWriter) WriteString(p string) (int, error) { return w.Write([]byte(p)) }
func TestUpgradeAnthropicCompatDisconnectStillCollectsFinalUsage(t *testing.T) {
	setGinTestMode()
	for _, responses := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/", nil)
		writer := &upgradeFailingWriter{ResponseWriter: c.Writer}
		c.Writer = writer
		body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_drain\",\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":7}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":99}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
		svc := &GatewayService{}
		var result *ForwardResult
		var err error
		if responses {
			result, err = svc.handleResponsesStreamingResponse(resp, c, "client-model", "upstream-model", nil, time.Now())
		} else {
			result, err = svc.handleCCStreamingFromAnthropic(resp, c, "client-model", "upstream-model", nil, time.Now(), true)
		}
		require.NoError(t, err)
		require.True(t, result.ClientDisconnect)
		require.False(t, result.FailedOutcome)
		require.Equal(t, 99, result.Usage.OutputTokens)
		require.Equal(t, 12, result.Usage.InputTokens)
		require.Equal(t, 7, result.Usage.CacheReadInputTokens)
		require.Equal(t, 1, writer.attempts, "no terminal frames after downstream disconnect")
	}
}

func TestUpgradeAnthropicCompatMultilineFramesStopWithoutWaitingForEOF(t *testing.T) {
	setGinTestMode()
	for _, responses := range []bool{false, true} {
		t.Run(map[bool]string{false: "chat_completions", true: "responses"}[responses], func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
			body := strings.Join([]string{
				": upstream comment",
				"event: message_start",
				`data: {"type":"message_start","message":`,
				`data: {"id":"msg_multiline","role":"assistant","content":[],"usage":{"input_tokens":5,"cache_read_input_tokens":2}}}`,
				"",
				"event: message_delta",
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
				"",
				"event: message_stop",
				`data: {"type":"message_stop"}`,
				"",
			}, "\n") + "\n"
			go func() { _, _ = io.WriteString(writer, body) }()

			done := make(chan struct{})
			var result *ForwardResult
			var err error
			go func() {
				defer close(done)
				resp := &http.Response{Header: http.Header{}, Body: reader}
				if responses {
					result, err = (&GatewayService{}).handleResponsesStreamingResponse(resp, c, "client", "upstream", nil, time.Now())
				} else {
					result, err = (&GatewayService{}).handleCCStreamingFromAnthropic(resp, c, "client", "upstream", nil, time.Now(), true)
				}
			}()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("valid message_stop must end the conversion without upstream EOF")
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 5, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.CacheReadInputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			if responses {
				require.Contains(t, rec.Body.String(), "response.completed")
			} else {
				require.Contains(t, rec.Body.String(), "data: [DONE]\n\n")
			}
		})
	}
}

func TestUpgradeGrokFastResourceAndCustomOverride(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json"))
	require.NoError(t, err)
	pricing := &PricingService{}
	pricing.pricingData, err = pricing.parsePricingData(body)
	require.NoError(t, err)
	dynamic := NewBillingService(&config.Config{}, pricing)
	fallback := newTestBillingService()
	for _, model := range []string{"grok-4.7-fast", "grok-4.7-build-fast", "xai/grok-4.7-fast", "x-ai/grok-4.7-build-fast", "grok/grok-4.7-fast"} {
		for _, input := range []int{199999, 200000, 200001} {
			tokens := UsageTokens{InputTokens: input - 100, CacheReadTokens: 100, OutputTokens: 100}
			want, err := fallback.CalculateCost(model, tokens, 1)
			require.NoError(t, err)
			got, err := dynamic.CalculateCost(model, tokens, 1)
			require.NoError(t, err)
			require.InDelta(t, want.TotalCost, got.TotalCost, 1e-10, "%s context=%d", model, input)
		}
	}
	custom := &ModelPricing{InputPricePerToken: 9e-6, OutputPricePerToken: 10e-6, PricingOverride: true, LongContextInputThreshold: 999, LongContextInputMultiplier: 3}
	require.Equal(t, custom, dynamic.applyModelSpecificPricingPolicy("grok-4.7-fast", custom))
	require.Equal(t, custom, dynamic.applyModelSpecificPricingPolicyEx("grok-4.7-fast", custom, false))
}
