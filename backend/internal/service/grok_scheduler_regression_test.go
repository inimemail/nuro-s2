package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokTextSchedulerPreservesEligibilityGates(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, batch := range []bool{false, true} {
			for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
				for _, gate := range []string{"text", "wrong_platform", "disabled", "model_mapping", "image_basic", "image_native", "cooldown"} {
					t.Run(fmt.Sprintf("advanced=%s/batch=%t/%s/%s", advanced, batch, accountType, gate), func(t *testing.T) {
						resetOpenAIAdvancedSchedulerSettingCacheForTest()
						t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
						groupID := int64(91077)
						account := Account{ID: 91077, Platform: PlatformGrok, Type: accountType,
							Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID}}
						platform := PlatformGrok
						imageCapability := OpenAIImagesCapability("")
						switch gate {
						case "wrong_platform":
							platform = PlatformOpenAI
						case "disabled":
							account.Schedulable = false
						case "model_mapping":
							account.Credentials = map[string]any{"model_mapping": map[string]any{"grok-other": "grok-other"}}
						case "image_basic":
							imageCapability = OpenAIImagesCapabilityBasic
						case "image_native":
							imageCapability = OpenAIImagesCapabilityNative
						case "cooldown":
							until := time.Now().Add(time.Hour)
							account.RateLimitResetAt = &until
						}
						cfg := &config.Config{}
						cfg.Gateway.Scheduling.LoadBatchEnabled = batch
						var released int64
						svc := &OpenAIGatewayService{
							accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{account}},
							cache:       &schedulerTestGatewayCache{}, cfg: cfg,
							rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced),
							concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{releasedAccountID: &released}),
						}
						selection, _, err := svc.selectAccountWithScheduler(context.Background(), &groupID, 0, 0, "", "", 0,
							"grok-4.6", nil, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
							imageCapability, false, platform, -1)
						if gate != "text" {
							require.Error(t, err)
							require.Nil(t, selection)
							return
						}
						require.NoError(t, err)
						require.NotNil(t, selection)
						require.Equal(t, account.ID, selection.Account.ID)
						require.True(t, selection.Acquired)
						require.NotNil(t, selection.ReleaseFunc)
						selection.ReleaseFunc()
						require.Equal(t, account.ID, released)
					})
				}
			}
		}
	}
}

func TestGrokScheduledResponsesReachesUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			groupID := int64(91078)
			account := Account{ID: 91078, Platform: PlatformGrok, Type: AccountTypeAPIKey,
				Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{groupID},
				Credentials: map[string]any{"api_key": "test-key"}}
			response := `{"id":"resp_test","object":"response","status":"completed","model":"grok-4.6","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
			contentType := "application/json"
			if stream {
				response = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
				contentType = "text/event-stream"
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response))}}
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{account}},
				cfg: &config.Config{}, cache: &schedulerTestGatewayCache{}, httpUpstream: upstream,
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{})}
			selection, _, err := svc.SelectAccountWithSchedulerForCapabilityOnPlatform(context.Background(), &groupID,
				"", "", "grok-4.6", nil, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, PlatformGrok)
			require.NoError(t, err)
			require.NotNil(t, selection)
			t.Cleanup(selection.ReleaseFunc)
			body := []byte(fmt.Sprintf(`{"model":"grok-4.6","input":"hi","stream":%t}`, stream))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
			result, err := svc.Forward(c.Request.Context(), c, selection.Account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)
			require.Equal(t, "grok-4.6", gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, "Bearer test-key", upstream.lastReq.Header.Get("Authorization"))
			require.Equal(t, http.StatusOK, w.Code)
			require.Contains(t, w.Body.String(), "Hello")
		})
	}
}

func TestAccountImageCapabilityKeepsOpenAIImageRestrictions(t *testing.T) {
	var missing *Account
	require.False(t, missing.SupportsOpenAIImageCapability(""))
	for _, platform := range []string{PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformDeepSeek, PlatformZhipu, PlatformMiniMax, PlatformOpenCodeGo} {
		for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
			account := &Account{Platform: platform, Type: accountType}
			require.True(t, account.SupportsOpenAIImageCapability(""), platform)
			for _, capability := range []OpenAIImagesCapability{OpenAIImagesCapabilityBasic, OpenAIImagesCapabilityNative} {
				require.Equal(t, platform == PlatformOpenAI, account.SupportsOpenAIImageCapability(capability), platform)
			}
		}
	}
}
