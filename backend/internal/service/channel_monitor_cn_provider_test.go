package service

import "testing"

func TestChannelMonitorCNProvidersUseOpenAICompatibleAdapter(t *testing.T) {
	expectedPaths := map[string]string{
		MonitorProviderKimi:     providerKimiPath,
		MonitorProviderZhipu:    providerZhipuPath,
		MonitorProviderDeepSeek: providerDeepSeekPath,
	}
	for _, provider := range []string{MonitorProviderKimi, MonitorProviderZhipu, MonitorProviderDeepSeek} {
		adapter, mode, ok := providerAdapterFor(provider, MonitorAPIModeChatCompletions)
		if !ok {
			t.Fatalf("provider %q was not registered", provider)
		}
		if mode != MonitorAPIModeChatCompletions {
			t.Fatalf("provider %q mode = %q", provider, mode)
		}
		body, err := adapter.buildBody("model-x", "1+1=?")
		if err != nil {
			t.Fatalf("provider %q build body: %v", provider, err)
		}
		if len(body) == 0 || len(adapter.buildHeaders("key")["Authorization"]) == 0 {
			t.Fatalf("provider %q did not produce a compatible request", provider)
		}
		if got := adapter.buildPath("model-x"); got != expectedPaths[provider] {
			t.Fatalf("provider %q path = %q, want %q", provider, got, expectedPaths[provider])
		}
		if err := validateAPIMode(provider, MonitorAPIModeResponses); err == nil {
			t.Fatalf("provider %q must reject Responses API mode", provider)
		}
	}
}

func TestChannelMonitorCNProvidersValidateThroughPublicEntryPoint(t *testing.T) {
	for _, provider := range []string{MonitorProviderKimi, MonitorProviderZhipu, MonitorProviderDeepSeek} {
		if err := validateProvider(provider); err != nil {
			t.Fatalf("validateProvider(%q): %v", provider, err)
		}
	}
}
