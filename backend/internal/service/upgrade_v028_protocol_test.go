package service

import (
	"context"
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestV028GPT6FinalBodySampling(t *testing.T) {
	for _, effort := range []string{"none", "high", "max"} {
		body := []byte(`{"model":"gpt-6-sol","reasoning":{"mode":"pro","effort":"` + effort + `"},"temperature":0.7,"top_p":0.9,"include":["message.output_text.logprobs","reasoning.encrypted_content"]}`)
		out, err := normalizeGPT6ResponsesSampling(body)
		require.NoError(t, err)
		require.Equal(t, "pro", gjson.GetBytes(out, "reasoning.mode").String())
		require.Equal(t, effort, gjson.GetBytes(out, "reasoning.effort").String())
		require.Equal(t, effort == "none", gjson.GetBytes(out, "temperature").Exists())
		var object map[string]any
		require.NoError(t, json.Unmarshal(out, &object))
		applyCodexOAuthTransform(object, false, false)
		_, sampling := object["temperature"]
		require.Equal(t, effort == "none", sampling)
	}
	old := []byte(`{"model":"gpt-5.6","temperature":0.7}`)
	out, err := normalizeGPT6ResponsesSampling(old)
	require.NoError(t, err)
	require.Equal(t, old, out)
}
func TestV028LiteMappingDoesNotReplayOrTrimHistory(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"` + responsesLiteWSMetadataKey + `":"true"},"input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com", strings.NewReader(string(body)))
	req.Header.Set(responsesLiteHeader, "true")
	require.NoError(t, applyMappedGPT55LiteCompatibility(req, &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, body))
	require.Nil(t, req.GetBody)
	require.Empty(t, req.Header.Get(responsesLiteHeader))
	out, _ := io.ReadAll(req.Body)
	require.Equal(t, "call_1", gjson.GetBytes(out, "input.0.call_id").String())
}
func TestV028TerminalFrameFinishesWithoutEOF(t *testing.T) {
	for _, mode := range []string{"normal", "passthrough", "normal-async", "passthrough-async"} {
		t.Run(mode, func(t *testing.T) {
			passthrough := strings.HasPrefix(mode, "passthrough")
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			svc := &OpenAIGatewayService{}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			if strings.HasSuffix(mode, "-async") {
				account.Extra = map[string]any{openAIAPIKeyFirstTokenTimeoutPlaceholderEnabledExtraKey: true, openAIAPIKeyFirstTokenTimeoutPlaceholderMsExtraKey: 100000}
			}
			resp := &http.Response{Header: make(http.Header), Body: reader, StatusCode: 200}
			finished := make(chan error, 1)
			go func() {
				if passthrough {
					_, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "alias", "gpt-6-sol")
					finished <- err
				} else {
					_, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "alias", "gpt-6-sol")
					finished <- err
				}
			}()
			_, err := io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"gpt-6-sol-build\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n")
			require.NoError(t, err)
			select {
			case <-finished:
				t.Fatal("terminated before complete frame")
			case <-time.After(10 * time.Millisecond):
			}
			_, err = io.WriteString(writer, "\n")
			require.NoError(t, err)
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("waited for upstream EOF")
			}
			require.True(t, strings.HasSuffix(rec.Body.String(), "\n\n"))
			require.Contains(t, rec.Body.String(), `"model":"alias"`)
		})
	}
}

func TestV028PingHeaderRetainsClassificationWithoutPublicPayload(t *testing.T) {
	state := &openAIUpstreamSSEState{}
	for _, event := range []string{"ping", "keepalive"} {
		line, keep := state.sanitizeControlLine("event: " + event)
		require.True(t, keep)
		require.Equal(t, ":", line)
		require.Equal(t, event, state.dataEventType([]byte(`{}`)))
		state.sanitizeControlLine("")
		require.Empty(t, state.dataEventType([]byte(`{}`)))
	}
}
func TestV028GeminiTransportDoesNotReplayUnknown(t *testing.T) {
	sent := &atomic.Bool{}
	dial := &net.OpError{Op: "dial", Err: &net.DNSError{Err: "missing"}}
	require.True(t, geminiTransportCanFailover(context.Background(), sent, dial))
	sent.Store(true)
	require.False(t, geminiTransportCanFailover(context.Background(), sent, dial))
	sent.Store(false)
	require.False(t, geminiTransportCanFailover(context.Background(), sent, io.EOF))
}

func TestV028ScopedImageErrorsAndNewModelToolGuard(t *testing.T) {
	for _, body := range []string{`{"error":{"code":"insufficient_balance"}}`, `{"error":{"details":[{"errorKey":"insufficient_balance"}]}}`} {
		require.True(t, isOpenAIImagesInsufficientBalance([]byte(body)))
	}
	for _, body := range []string{`{"prompt":"insufficient_balance"}`, `{"error":{"message":"insufficient_balance"}}`, `{"data":{"code":"insufficient_balance"}}`} {
		require.False(t, isOpenAIImagesInsufficientBalance([]byte(body)))
	}
	require.Error(t, validateNewModelChatReasoningTools("gpt-6-sol", "high", true))
	require.NoError(t, validateNewModelChatReasoningTools("gpt-6-sol", "none", true))
	require.NoError(t, validateNewModelChatReasoningTools("gpt-5.6", "high", true))
	require.Error(t, validateNewModelChatReasoningTools("claude-opus-5-5", "", true))
}

func TestV028DeepSeekImageAliasesDoNotTouchToolArguments(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}},{"type":"input_image","file_id":"file-1"}]},{"type":"function_call","arguments":"{\"image_url\":\"untouched\"}"}]}`)
	a := &Account{Platform: PlatformDeepseek, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_protocol": "responses"}}
	out := normalizeDeepSeekResponsesRequestBody(a, body)
	require.Equal(t, "data:image/png;base64,abc", gjson.GetBytes(out, "input.0.content.0.url").String())
	require.Equal(t, "file-1", gjson.GetBytes(out, "input.0.content.1.file_id").String())
	require.Equal(t, gjson.GetBytes(body, "input.1.arguments").String(), gjson.GetBytes(out, "input.1.arguments").String())
	a.Platform = PlatformKimi
	require.Equal(t, body, normalizeDeepSeekResponsesRequestBody(a, body))
}
func TestV028NewModelPriceBoundaries(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
		for _, input := range []int{271999, 272000, 272001} {
			cost, err := svc.CalculateCost(model, UsageTokens{InputTokens: input, OutputTokens: 1000}, 1)
			require.NoError(t, err)
			require.Equal(t, input > 272000, cost.LongContextBillingApplied)
		}
	}
	for _, model := range []string{"grok-4.7", "grok-4.7-latest"} {
		for _, input := range []int{199999, 200000, 200001} {
			cost, err := svc.CalculateCost(model, UsageTokens{InputTokens: input, OutputTokens: 1000}, 1)
			require.NoError(t, err)
			factor := 1.0
			if input >= 200000 {
				factor = 2
			}
			require.InDelta(t, (float64(input)*2e-6+1000*6e-6)*factor, cost.TotalCost, 1e-9)
		}
	}
}

func TestV028CatalogPricesMatchFallbackAndPriorityCacheBuckets(t *testing.T) {
	fallback := NewBillingService(&config.Config{}, nil)
	catalog := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"grok-4.7":        {InputCostPerToken: 2e-6, OutputCostPerToken: 6e-6, CacheReadInputTokenCost: 0.5e-6},
		"claude-opus-5-5": {InputCostPerToken: 4e-6, OutputCostPerToken: 20e-6, CacheReadInputTokenCost: 0.2e-6, CacheCreationInputTokenCost: 5e-6, CacheCreationInputTokenCostAbove1hr: 8e-6, InputCostPerTokenPriority: 8e-6, OutputCostPerTokenPriority: 40e-6, CacheReadInputTokenCostPriority: 0.4e-6, CacheCreationInputTokenCostPriority: 10e-6},
	}}
	dynamic := NewBillingService(&config.Config{}, catalog)
	for _, input := range []int{199899, 199900, 199901} {
		tokens := UsageTokens{InputTokens: input, CacheReadTokens: 100, OutputTokens: 30}
		a, err := fallback.CalculateCost("grok-4.7", tokens, 1)
		require.NoError(t, err)
		b, err := dynamic.CalculateCost("grok-4.7", tokens, 1)
		require.NoError(t, err)
		require.Equal(t, a, b)
	}
	for _, svc := range []*BillingService{fallback, dynamic} {
		tokens := UsageTokens{InputTokens: 100, OutputTokens: 100, CacheReadTokens: 100, CacheCreationTokens: 200, CacheCreation5mTokens: 100, CacheCreation1hTokens: 100}
		base, err := svc.CalculateCost("claude-opus-5-5", tokens, 1)
		require.NoError(t, err)
		priority, err := svc.CalculateCostWithServiceTier("claude-opus-5-5", tokens, 1, "priority")
		require.NoError(t, err)
		require.InDelta(t, 0.0013, base.CacheCreationCost, 1e-12)
		require.InDelta(t, 0.0026, priority.CacheCreationCost, 1e-12)
		require.InDelta(t, base.TotalCost*2, priority.TotalCost, 1e-12)
	}
}
