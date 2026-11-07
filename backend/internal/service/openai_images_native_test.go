package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeImages26ForwardUsesOneVerifiedMediaAttempt(t *testing.T) {
	for _, typ := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, endpoint := range []string{openAIImagesGenerationsEndpoint, openAIImagesEditsEndpoint} {
			t.Run(typ+endpoint, func(t *testing.T) {
				body := []byte(`{"model":"gpt-image-alias","prompt":"  draw cat  ","stream":true,"n":2,"complexity":"high","output_format":"svg","svg":{"model":"gpt-6.1-sol","reasoning":{"effort":"high"}},"images":[{"image_url":"https://example.com/input.png"}],"markup":{"image_url":"https://example.com/markup.png"},"response_format":"b64_json","untrusted":"drop"}`)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", endpoint, bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"created":1,"output_format":"svg","usage":{"input_tokens":100,"input_tokens_details":{"image_tokens":60,"cached_tokens":30,"cached_tokens_details":{"image_tokens":20,"text_tokens":10}},"output_tokens":10},"data":[{"b64_json":"PHN2Zy8+"},{"b64_json":"PHN2Zy8+"}]}`))}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				parsed, err := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				account := &Account{ID: 91, Platform: PlatformOpenAI, Type: typ, Credentials: map[string]any{"api_key": "fake", "access_token": "fake", "base_url": "https://images.example/v1", "model_mapping": map[string]any{"gpt-image-alias": "gpt-image-2.6-flare"}}}
				result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
				require.NoError(t, err)
				require.NotNil(t, result)
				wantURL := "https://images.example" + endpoint
				if typ == AccountTypeOAuth {
					wantURL = "https://chatgpt.com/backend-api/codex" + strings.TrimPrefix(endpoint, "/v1")
				}
				require.Equal(t, wantURL, upstream.lastReq.URL.String())
				require.Equal(t, HTTPUpstreamProfileOpenAIMedia, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
				require.Equal(t, "gpt-image-2.6-flare", gjson.GetBytes(upstream.lastBody, "model").String())
				require.Equal(t, "  draw cat  ", gjson.GetBytes(upstream.lastBody, "prompt").String())
				require.False(t, gjson.GetBytes(upstream.lastBody, "response_format").Exists())
				require.False(t, gjson.GetBytes(upstream.lastBody, "client_metadata").Exists())
				require.False(t, gjson.GetBytes(upstream.lastBody, "untrusted").Exists())
				require.NotEmpty(t, result.AttemptID)
				require.True(t, result.UpstreamRequestBodyStarted)
				require.Equal(t, 2, result.ImageCount)
				require.Equal(t, 20, result.Usage.ImageCacheReadTokens)
				require.Equal(t, 10, result.Usage.ImageOutputTokens)
				events := parseOpenAIImageTestSSEEvents(rec.Body.String())
				require.Len(t, events, 2)
				require.False(t, gjson.Get(events[0].Data, "usage").Exists())
				require.Equal(t, int64(100), gjson.Get(events[1].Data, "usage.input_tokens").Int())
			})
		}
	}
}
func TestNativeImages26RejectsBeforeSending(t *testing.T) {
	for _, field := range []string{`"frame_count":0`, `"frame_count":1.5`, `"frame_duration_ms":2147483648`, `"complexity":null`, `"complexity":"max"`, `"svg":{"model":"x","unexpected":true}`, `"svg":{"model":"x","reasoning":{"effort":"ultra"}}`, `"markup":{"file_id":"file-x"}`} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"cat",` + field + `}`)
		c.Request = httptest.NewRequest("POST", openAIImagesGenerationsEndpoint, bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		_, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, body)
		require.Error(t, err, field)
	}
	require.Error(t, validateNativeImagesRoute(&Account{Extra: map[string]any{"openai_responses_mode": "force_responses"}}, "gpt-image-2.6-flare"))
}
func TestNativeImages26MultipartSVGRequiresOneJSONObject(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"object", `{"model":"gpt-6.1-sol","reasoning":{"effort":"high"}}`, true},
		{"whitespace", " \n{\"model\":\"gpt-6.1-sol\"}\t ", true},
		{"second_object", `{"model":"gpt-6.1-sol"} {"unexpected":true}`, false},
		{"second_scalar", `{"model":"gpt-6.1-sol"} null`, false},
		{"trailing_garbage", `{"model":"gpt-6.1-sol"} invalid`, false},
		{"unknown_field", `{"model":"gpt-6.1-sol","unexpected":true}`, false},
		{"array", `[{"model":"gpt-6.1-sol"}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for field, value := range map[string]string{"model": "gpt-image-2.6-flare", "prompt": "fixture", "svg": tc.raw} {
				require.NoError(t, writer.WriteField(field, value))
			}
			require.NoError(t, writer.Close())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", openAIImagesGenerationsEndpoint, bytes.NewReader(body.Bytes()))
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			parsed, err := (&OpenAIGatewayService{}).ParseOpenAIImagesRequest(c, body.Bytes())
			if !tc.valid {
				require.ErrorContains(t, err, "invalid svg author options")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, parsed.SVG)
			require.Equal(t, "gpt-6.1-sol", parsed.SVG.Model)
		})
	}
}

func TestNativeImages26PricesImageCacheWithoutMutatingRawUsage(t *testing.T) {
	raw := []byte(`{"usage":{"input_tokens":100,"input_tokens_details":{"image_tokens":60,"cached_tokens":30,"cached_tokens_details":{"image_tokens":20,"text_tokens":10}},"output_tokens":10}}`)
	usage, ok := nativeOpenAIImagesUsage(raw)
	require.True(t, ok)
	original := usage
	svc := &BillingService{}
	p, err := svc.GetModelPricing("gpt-image-2.6-sunburst")
	require.NoError(t, err)
	cost := svc.computeTokenBreakdown(p, UsageTokens{InputTokens: 70, ImageInputTokens: 40, CacheReadTokens: 30, ImageCacheReadTokens: 20, OutputTokens: 10, ImageOutputTokens: 10}, 1, "", false)
	require.InDelta(t, 30*5e-6+40*8e-6+10*1.25e-6+20*2e-6+10*30e-6, cost.TotalCost, 1e-12)
	require.Equal(t, original, usage)
}
func TestNativeImagesSafeDimensions(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{`<svg xmlns="http://www.w3.org/2000/svg" width="640px" height="480"/>`, "640x480"}, {`<svg viewBox="0 0 120 80"/>`, "120x80"}, {`<svg width="1e100" height="10"/>`, ""}, {`<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///etc/passwd">]><svg width="&x;" height="1"/>`, ""}} {
		require.Equal(t, tc.want, openAIImageInlineSize(base64.StdEncoding.EncodeToString([]byte(tc.raw))))
	}
	require.Equal(t, "64x32", openAIImageInlineSize(base64.StdEncoding.EncodeToString([]byte("GIF89a\x40\x00\x20\x00\x00\x00\x00"))))
}

func TestNativeImagesCacheRateOverridesIncludingZero(t *testing.T) {
	svc := &BillingService{}
	tokens := UsageTokens{CacheReadTokens: 30, ImageCacheReadTokens: 20}
	base, err := svc.GetModelPricing("gpt-image-2.6-flare")
	require.NoError(t, err)
	for _, rate := range []float64{0, 7e-6} {
		for _, tier := range []string{"", "priority"} {
			channel, err := svc.GetModelPricingWithChannel("gpt-image-2.6-flare", &ChannelModelPricing{CacheReadPrice: &rate})
			require.NoError(t, err)
			require.InDelta(t, 30*rate, svc.computeTokenBreakdown(channel, tokens, 1, tier, false).CacheReadCost, 1e-12)
			resolved := &ResolvedPricing{BasePricing: base}
			(&ModelPricingResolver{}).applyTokenOverrides(&ChannelModelPricing{CacheReadPrice: &rate}, resolved)
			require.InDelta(t, 30*rate, svc.computeTokenBreakdown(resolved.BasePricing, tokens, 1, tier, false).CacheReadCost, 1e-12)
			interval := intervalToModelPricing(&PricingInterval{CacheReadPrice: &rate}, base, false, nil)
			require.InDelta(t, 30*rate, svc.computeTokenBreakdown(interval, tokens, 1, tier, false).CacheReadCost, 1e-12)
		}
	}
	zero := 0.0
	interval := intervalToModelPricing(&PricingInterval{CacheReadMultiplier: &zero}, base, false, nil)
	require.Zero(t, svc.computeTokenBreakdown(interval, tokens, 1, "", false).CacheReadCost)
	require.Equal(t, 2e-6, base.ImageCacheReadPricePerToken)
	data, err := (&PricingService{}).parsePricingData([]byte(`{"custom-image":{"input_cost_per_token":0.000005,"cache_read_input_token_cost":0.000001,"cache_read_input_image_token_cost":0}}`))
	require.NoError(t, err)
	customSvc := &BillingService{pricingService: &PricingService{pricingData: data}}
	custom, err := customSvc.GetModelPricing("custom-image")
	require.NoError(t, err)
	require.True(t, custom.ImageCacheReadPriceExplicit)
	require.InDelta(t, 10e-6, svc.computeTokenBreakdown(custom, tokens, 1, "", false).CacheReadCost, 1e-12)
}

func TestNativeImages26MultipartEditMaskMarkupAndGIFFallback(t *testing.T) {
	for _, typ := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		t.Run(typ, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			for field, value := range map[string]string{"model": "gpt-image-2.6-flare", "prompt": "edit fixture", "stream": "true", "output_format": "gif", "frame_count": "4", "frame_duration_ms": "80", "complexity": "high"} {
				require.NoError(t, writer.WriteField(field, value))
			}
			for _, field := range []string{"image", "mask", "markup"} {
				header := make(textproto.MIMEHeader)
				header.Set("Content-Disposition", `form-data; name="`+field+`"; filename="`+field+`.png"`)
				header.Set("Content-Type", "image/png")
				part, err := writer.CreatePart(header)
				require.NoError(t, err)
				_, err = part.Write([]byte(field + "-fixture"))
				require.NoError(t, err)
			}
			require.NoError(t, writer.Close())
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", openAIImagesEditsEndpoint, bytes.NewReader(body.Bytes()))
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			gif := base64.StdEncoding.EncodeToString([]byte("GIF89a\x40\x00\x20\x00\x00\x00\x00"))
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(`{"output_format":"gif","usage":{"input_tokens":100,"output_tokens":10},"data":[{"b64_json":"` + gif + `"}]}`))}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			parsed, err := svc.ParseOpenAIImagesRequest(c, body.Bytes())
			require.NoError(t, err)
			account := &Account{ID: 93, Platform: PlatformOpenAI, Type: typ, Credentials: map[string]any{"api_key": "fixture", "access_token": "fixture", "base_url": "https://images.example/v1"}}
			result, err := svc.ForwardImages(t.Context(), c, account, body.Bytes(), parsed, "")
			require.NoError(t, err)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, 1, result.ImageCount)
			require.Equal(t, 100, result.Usage.InputTokens)
			require.Equal(t, 10, result.Usage.OutputTokens)
			require.Contains(t, w.Body.String(), gif)
			require.Equal(t, "application/json", upstream.lastReq.Header.Get("Content-Type"))
			for _, field := range []string{"mask", "markup"} {
				require.Equal(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte(field+"-fixture")), gjson.GetBytes(upstream.lastBody, field+".image_url").String())
			}
			require.Equal(t, int64(4), gjson.GetBytes(upstream.lastBody, "frame_count").Int())
			require.Contains(t, w.Body.String(), "image_edit.completed")
		})
	}
}

func TestNativeImages26AgentAssertionAndSetupCredentialBoundary(t *testing.T) {
	key, private := newTestAgentIdentityKey(t)
	account := &Account{ID: 94, Type: AccountTypeOAuth, Platform: PlatformOpenAI, Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity, "agent_runtime_id": key.runtimeID, "agent_private_key": private, "task_id": key.taskID}}
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"R0lG"}]}`))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"fixture"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", openAIImagesGenerationsEndpoint, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	_, err = svc.ForwardImages(t.Context(), c, account, body, parsed, "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.True(t, strings.HasPrefix(upstream.lastReq.Header.Get("Authorization"), "AgentAssertion "))
	require.NotContains(t, string(upstream.lastBody), private)
	account.Type = AccountTypeSetupToken
	_, err = svc.ForwardImages(t.Context(), c, account, body, parsed, "")
	require.ErrorContains(t, err, "unsupported account type")
	require.Len(t, upstream.requests, 1)
}

func TestNativeImages26UnknownAfterBodyIsNotReplayable(t *testing.T) {
	upstream := &httpUpstreamRecorder{err: io.ErrUnexpectedEOF}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"fixture"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", openAIImagesGenerationsEndpoint, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	_, err = svc.ForwardImages(t.Context(), c, &Account{ID: 95, Type: AccountTypeAPIKey, Platform: PlatformOpenAI, Credentials: map[string]any{"api_key": "fixture", "pool_mode": true}}, body, parsed, "")
	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
	var failover *UpstreamFailoverError
	require.True(t, errors.As(err, &failover))
	require.True(t, failover.ExecutionUnknown)
	require.False(t, failover.RetryableOnSameAccount)
}

func TestNativeImageCacheCostLongContextSplitConservesTokens(t *testing.T) {
	svc := &BillingService{}
	for _, threshold := range []int{15, 25, 30} {
		cost, err := svc.CalculateCostWithLongContext("gpt-image-2.6-flare", UsageTokens{CacheReadTokens: 30, ImageCacheReadTokens: 20}, 1, threshold, 2)
		require.NoError(t, err)
		inImages := 20 * threshold / 30
		inText := threshold - inImages
		outImages := 20 - inImages
		outText := 10 - inText
		want := float64(inImages)*2e-6 + float64(inText)*1.25e-6 + 2*(float64(outImages)*2e-6+float64(outText)*1.25e-6)
		// Range multipliers affect ActualCost; the provider cost breakdown
		// continues to preserve the unmultiplied token prices.
		require.InDelta(t, 20*2e-6+10*1.25e-6, cost.CacheReadCost, 1e-12)
		require.InDelta(t, want, cost.ActualCost, 1e-12)
	}
}

func TestNativeImageCacheCostSessionLongContextPreservesPricing(t *testing.T) {
	svc := &BillingService{}
	pricing := &ModelPricing{CacheReadPricePerToken: 1.25e-6, ImageCacheReadPricePerToken: 2e-6, ImageCacheReadPriceExplicit: true, LongContextInputThreshold: 25, LongContextInputMultiplier: 2}
	tokens := UsageTokens{CacheReadTokens: 30, ImageCacheReadTokens: 20}
	for _, tier := range []string{"", "priority", "flex"} {
		base := svc.computeTokenBreakdown(pricing, tokens, 1, tier, false)
		cost := svc.computeTokenBreakdown(pricing, tokens, 1.5, tier, true)
		require.InDelta(t, 2*base.CacheReadCost, cost.CacheReadCost, 1e-12)
		require.InDelta(t, 3*base.TotalCost, cost.ActualCost, 1e-12)
		require.Equal(t, 2e-6, pricing.ImageCacheReadPricePerToken)
	}
	pricing.ImageCacheReadPricePerToken = 0
	cost := svc.computeTokenBreakdown(pricing, tokens, 1, "", true)
	require.InDelta(t, 10*1.25e-6*2, cost.CacheReadCost, 1e-12)
}
