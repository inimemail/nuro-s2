package service

import "testing"

func TestResolveThinkingProtocol(t *testing.T) {
	tests := []struct {
		name    string
		modelID string
		want    ThinkingProtocol
	}{
		{"claude sonnet", "claude-sonnet-4-5", ThinkingProtocolAnthropicStrict},
		{"claude opus full", "claude-opus-4-5-20251101", ThinkingProtocolAnthropicStrict},
		{"short opus", "opus-4-5", ThinkingProtocolAnthropicStrict},
		{"short sonnet", "sonnet-4-5", ThinkingProtocolAnthropicStrict},
		{"short haiku", "haiku-4-5", ThinkingProtocolAnthropicStrict},
		{"deepseek", "deepseek-v4-pro", ThinkingProtocolPassbackRequired},
		{"kimi", "kimi-coding-v2", ThinkingProtocolPassbackRequired},
		{"moonshot", "moonshot-v1-32k", ThinkingProtocolPassbackRequired},
		{"glm", "glm-5.1", ThinkingProtocolPassbackRequired},
		{"minimax", "MiniMax-M2.7-highspeed", ThinkingProtocolPassbackRequired},
		{"qwen thinking", "qwen3-235b-a22b-thinking-2507", ThinkingProtocolPassbackRequired},
		{"qwen non thinking", "qwen3-32b", ThinkingProtocolUnknown},
		{"gpt", "gpt-5.1", ThinkingProtocolUnknown},
		{"gemini", "gemini-3-pro-preview", ThinkingProtocolUnknown},
		{"empty", "", ThinkingProtocolUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveThinkingProtocol(tt.modelID); got != tt.want {
				t.Fatalf("ResolveThinkingProtocol(%q) = %v, want %v", tt.modelID, got, tt.want)
			}
		})
	}
}

func TestShouldThinkingRetryFiltersStayStrictOnly(t *testing.T) {
	models := []string{
		"claude-sonnet-4-5",
		"deepseek-v4-pro",
		"kimi-coding",
		"glm-5.1",
		"qwen3-235b-a22b-thinking-2507",
		"gpt-5.1",
		"",
	}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			if ShouldApplyRetryFilters(model) != ShouldPreFilterThinkingBlocks(model) {
				t.Fatalf("retry filter decision diverged from pre-filter for %q", model)
			}
		})
	}
	if !ShouldRectifyThinkingSignatureError("claude-sonnet-4-5") {
		t.Fatal("claude strict models should allow signature rectifier")
	}
	if ShouldRectifyThinkingSignatureError("deepseek-v4-pro") {
		t.Fatal("passback-required models must not run signature rectifier")
	}
}
