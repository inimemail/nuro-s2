package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenCodeProtocolDefaultsAndMode(t *testing.T) {
	zen := &Account{Platform: PlatformOpenCodeGo, Credentials: map[string]any{"account_mode": AccountModeZen}}
	goPlan := &Account{Platform: PlatformOpenCodeGo, Extra: map[string]any{"account_mode": AccountModeGo}}
	require.True(t, zen.IsOpenCodeZen())
	require.True(t, goPlan.IsOpenCodeGoPlan())
	require.Equal(t, APIProtocolResponses, zen.ResolveOpenCodeGoUpstreamProtocol("gpt-5.6-luna"))
	require.Equal(t, APIProtocolAnthropic, zen.ResolveOpenCodeGoUpstreamProtocol("claude-sonnet-4-6"))
	require.Equal(t, APIProtocolAnthropic, goPlan.ResolveOpenCodeGoUpstreamProtocol("qwen3.8-max"))
	require.Equal(t, APIProtocolChatCompletions, goPlan.ResolveOpenCodeGoUpstreamProtocol("glm-5.3"))
}

func TestOpenCodeProtocolRulesAreStrictAndPinnedWins(t *testing.T) {
	account := &Account{Platform: PlatformOpenCodeGo, Extra: map[string]any{cnAPIProtocolExtraKey: APIProtocolResponses}, Credentials: map[string]any{
		openCodeGoProtocolRulesKey: []any{map[string]any{"pattern": "claude-*", "protocol": APIProtocolChatCompletions}},
	}}
	require.Equal(t, APIProtocolResponses, account.ResolveOpenCodeGoUpstreamProtocol("claude-sonnet"))
	bad := map[string]any{openCodeGoProtocolRulesKey: []any{map[string]any{"pattern": "a*b", "protocol": APIProtocolResponses}}}
	require.Error(t, NormalizeOpenCodeProtocolRulesCredentials(bad))
}

func TestOllamaCloudUsageExhaustionRequiresFreshKnownResets(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	snapshot := &OllamaCloudUsageSnapshot{Status: "ok", FetchedAt: opencodePtrTime(now), Data: &OllamaCloudUsageData{
		FiveHour: &OllamaCloudUsageWindow{UsedPercent: 100, ResetAt: &future},
	}}
	got, ok := ollamaCloudUsageExhaustionResetAt(snapshot, now, now.Add(-time.Minute))
	require.True(t, ok)
	require.Equal(t, future, got)
	snapshot.Data.FiveHour.ResetAt = nil
	_, ok = ollamaCloudUsageExhaustionResetAt(snapshot, now, now.Add(-time.Minute))
	require.False(t, ok)
}

func opencodePtrTime(t time.Time) *time.Time { return &t }
