package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIEdgeCacheCreationCapabilityPreservesLegacyAccounts(t *testing.T) {
	account := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{
		"openai_prompt_cache_creation_optimization_enabled": true,
	}}
	require.False(t, openAIEdgeCacheCreationPolicyCompatible(account, 0), "an old Edge must fall back to Go before handling the expanded policy")
	require.True(t, openAIEdgeCacheCreationPolicyCompatible(account, 1))
	account.Credentials["openai_prompt_cache_creation_optimization_enabled"] = false
	require.True(t, openAIEdgeCacheCreationPolicyCompatible(account, 0), "ordinary accounts retain their existing Edge route")
}

func TestOpenAIEdgeCacheCreationOnlyDWebSocketRequiresGo(t *testing.T) {
	for _, accountType := range []string{service.AccountTypeOAuth, service.AccountTypeAPIKey} {
		for _, mode := range []string{"reduce", "suppress", "free", "input_125"} {
			for _, enabled := range []bool{false, true} {
				account := &service.Account{Platform: service.PlatformOpenAI, Type: accountType, Credentials: map[string]any{
					"openai_prompt_cache_creation_optimization_enabled": enabled,
					"openai_prompt_cache_creation_optimization_mode":    mode,
				}}
				want := ""
				if enabled && mode == "input_125" {
					want = "cache_creation_pricing_requires_go_ws"
				}
				require.Equal(t, want, openAIEdgeResponsesWSAccountFallbackReason(account), "%s/%s/%t", accountType, mode, enabled)
				if accountType == service.AccountTypeAPIKey {
					require.Empty(t, openAIEdgeResponsesHTTPAccountFallbackReason(account), "HTTP Edge remains available for D")
				}
			}
		}
	}
}

func TestOpenAIEdgeCacheCreationLaterTurnRecordsActualModel(t *testing.T) {
	account := &service.Account{ID: 101, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{
		"openai_prompt_cache_creation_optimization_enabled": true,
	}}
	gateway := &service.OpenAIGatewayService{}
	h := &OpenAIGatewayHandler{gatewayService: gateway, openAIEdgeLeases: map[string]*openAIEdgeLease{"lease-1": {
		leaseID: "lease-1", account: account, cachePolicyEnabled: true,
	}}}
	body := json.RawMessage(`{"type":"response.create","model":"later-model","prompt_cache_options":{"mode":"explicit","ttl":"24h"}}`)
	c, _ := newOpenAIEdgeTestContext(http.MethodPost, "/internal/edge/openai/retry", `{}`, "")
	decision := h.openAIEdgeRetryDecision(c, service.OpenAIEdgeRetryRequest{LeaseID: "lease-1", AccountID: account.ID, UpstreamStatusCode: http.StatusBadRequest,
		RequestBody: body, ResponseBody: json.RawMessage(`{"error":{"message":"Unsupported parameter: prompt_cache_options"}}`), WroteClientResponse: true})
	require.Equal(t, "client_response_already_written", decision.Reason)
	require.False(t, gateway.IsOpenAIPromptCacheCreationOptimizationRuntimeEnabled(account, service.OpenAIPromptCacheCreationOptimizationScope("later-model", body)))
	updated, result, err := gateway.ApplyOpenAIPromptCacheCreationOptimizationBody(account, "other-model", []byte(`{"model":"other-model"}`))
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.NotEmpty(t, updated)
}
