package service

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestOpenAIEdgeRawRelayEligibleForInboundEndpoint(t *testing.T) {
	rawChatAccount := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesSupported: false,
		},
	}
	rawResponsesAccount := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			"openai_passthrough":                     true,
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}

	if !IsOpenAIEdgeRawRelayEligibleForInboundEndpoint(rawChatAccount, "/v1/chat/completions") {
		t.Fatal("expected raw chat account to be eligible for chat completions relay")
	}
	if IsOpenAIEdgeRawRelayEligibleForInboundEndpoint(rawChatAccount, "/v1/responses") {
		t.Fatal("expected raw chat account to be ineligible for responses relay")
	}
	if !IsOpenAIEdgeRawRelayEligibleForInboundEndpoint(rawResponsesAccount, "/v1/responses") {
		t.Fatal("expected passthrough responses account to be eligible for responses relay")
	}
	if IsOpenAIEdgeRawRelayEligibleForInboundEndpoint(rawResponsesAccount, "/v1/chat/completions") {
		t.Fatal("expected responses account to be ineligible for raw chat relay")
	}
	if got := OpenAIEdgeRawUpstreamEndpointForInbound(rawResponsesAccount, "/v1/responses"); got != "/v1/responses" {
		t.Fatalf("expected responses upstream endpoint, got %q", got)
	}
}

func TestOpenAIEdgeStrongIsolationBodyHelpers(t *testing.T) {
	chatBody := []byte(`{"model":"gpt-4.1","stream":true,"messages":[],"conversation_id":"conv","session_id":"sess","previous_response_id":"resp","store":true}`)
	isolatedChat, changed, err := applyOpenAIUpstreamStrongIsolationBody(chatBody, false)
	if err != nil {
		t.Fatalf("isolate chat body: %v", err)
	}
	if !changed {
		t.Fatal("expected chat body isolation to change body")
	}
	for _, field := range []string{"conversation_id", "session_id", "previous_response_id"} {
		if gjson.GetBytes(isolatedChat, field).Exists() {
			t.Fatalf("expected %s to be removed from chat body: %s", field, string(isolatedChat))
		}
	}
	if got := gjson.GetBytes(isolatedChat, "store").Bool(); got {
		t.Fatalf("expected store=false in isolated chat body: %s", string(isolatedChat))
	}

	wsBody := []byte(`{"type":"response.create","model":"gpt-4.1","store":true,"client_metadata":{"x-codex-turn-metadata":"m","x-codex-turn-state":"s","keep":"ok"}}`)
	isolatedWS, changed, err := applyOpenAIUpstreamStrongIsolationWSBody(wsBody, true)
	if err != nil {
		t.Fatalf("isolate ws body: %v", err)
	}
	if !changed {
		t.Fatal("expected ws body isolation to change body")
	}
	if gjson.GetBytes(isolatedWS, "client_metadata.x-codex-turn-metadata").Exists() ||
		gjson.GetBytes(isolatedWS, "client_metadata.x-codex-turn-state").Exists() {
		t.Fatalf("expected codex turn metadata fields to be removed: %s", string(isolatedWS))
	}
	if gjson.GetBytes(isolatedWS, "client_metadata").Exists() {
		t.Fatalf("expected client_metadata to be removed from isolated ws body: %s", string(isolatedWS))
	}
	if got := gjson.GetBytes(isolatedWS, "store").Bool(); got {
		t.Fatalf("expected store=false in isolated ws body: %s", string(isolatedWS))
	}
}

func TestScrubOpenAIEdgeStrongIsolationHeaders(t *testing.T) {
	headers := map[string]string{
		"Authorization":          "Bearer token",
		"conversation_id":        "conv",
		"Session_ID":             "sess",
		"x-codex-turn-state":     "state",
		"x-codex-turn-metadata":  "metadata",
		"originator":             "origin",
		"Accept":                 "text/event-stream",
		"X-Keep-This-Test-Value": "ok",
	}

	scrubOpenAIEdgeStrongIsolationHeaders(headers)

	for _, key := range []string{"conversation_id", "Session_ID", "x-codex-turn-state", "x-codex-turn-metadata", "originator"} {
		if _, ok := headers[key]; ok {
			t.Fatalf("expected %s to be removed from headers: %#v", key, headers)
		}
	}
	if headers["Authorization"] == "" || headers["Accept"] == "" || headers["X-Keep-This-Test-Value"] != "ok" {
		t.Fatalf("expected non-isolation headers to remain: %#v", headers)
	}
}

func TestBuildChatGPTOAuthResponsesEdgePlan(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "Mozilla/5.0")
	c.Request.Header.Set("conversation_id", "client-conv")
	c.Set("api_key", &APIKey{ID: 42})

	account := &Account{
		ID:       123,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-account",
		},
	}
	body := []byte(`{"model":"gpt-5","stream":true,"prompt_cache_key":"turn-1","input":[{"role":"user","content":"hi"}]}`)

	plan, err := (&OpenAIGatewayService{}).BuildChatGPTOAuthResponsesEdgePlan(context.Background(), c, account, body)
	if err != nil {
		t.Fatalf("build oauth edge plan: %v", err)
	}
	if plan.Plan.Action != OpenAIEdgeActionRelay || plan.Plan.Transport != OpenAIEdgeTransportHTTP2SSE {
		t.Fatalf("unexpected relay plan: %#v", plan.Plan)
	}
	if plan.Plan.UpstreamURL != chatgptCodexURL {
		t.Fatalf("unexpected upstream url: %q", plan.Plan.UpstreamURL)
	}
	if got := plan.Plan.Headers["Authorization"]; got != "Bearer oauth-token" {
		t.Fatalf("unexpected authorization header: %q", got)
	}
	if got := plan.Plan.Headers["Chatgpt-Account-Id"]; got != "chatgpt-account" {
		t.Fatalf("unexpected chatgpt account header: %#v", plan.Plan.Headers)
	}
	if got := plan.Plan.Headers["Accept"]; got != "text/event-stream" {
		t.Fatalf("unexpected accept header: %q", got)
	}
	if got := plan.Plan.Headers["Openai-Beta"]; got != "responses=experimental" {
		t.Fatalf("unexpected beta header: %#v", plan.Plan.Headers)
	}
	if got := plan.Plan.Headers["Originator"]; got != "opencode" {
		t.Fatalf("unexpected originator header: %q", got)
	}
	expectedSession := isolateOpenAISessionID(42, "turn-1")
	if got := plan.Plan.Headers["Session_id"]; got != expectedSession {
		t.Fatalf("unexpected session header: got %q want %q", got, expectedSession)
	}
	if got := plan.Plan.Headers["Conversation_id"]; got != expectedSession {
		t.Fatalf("unexpected conversation header: got %q want %q", got, expectedSession)
	}
	if got := plan.Plan.Headers["User-Agent"]; got != DefaultOpenAICodexUserAgent {
		t.Fatalf("browser user-agent should be replaced, got %q", got)
	}
	decoded, err := base64.StdEncoding.DecodeString(plan.Plan.BodyRawBase64)
	if err != nil {
		t.Fatalf("decode body_raw_base64: %v", err)
	}
	if model := gjson.GetBytes(decoded, "model").String(); model == "" {
		t.Fatalf("expected encoded body to decode to json: %s", string(decoded))
	}
	if model := gjson.GetBytes(plan.Plan.Body, "model").String(); model == "" {
		t.Fatalf("expected response body to remain json: %s", string(plan.Plan.Body))
	}
}
