package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
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
