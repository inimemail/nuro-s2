package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeReasoningEffortPolicy(t *testing.T) {
	require.Equal(t, "xhigh", NormalizeMaxReasoningEffort(" x-high "))
	require.Equal(t, "max", NormalizeMaxReasoningEffort("MAX"))
	require.Empty(t, NormalizeMaxReasoningEffort("future"))

	got, err := NormalizeReasoningEffortMappings(PlatformOpenAI, []ReasoningEffortMapping{
		{From: " MAX ", To: "x-high"},
	})
	require.NoError(t, err)
	require.Equal(t, []ReasoningEffortMapping{{From: "max", To: "xhigh"}}, got)

	_, err = NormalizeReasoningEffortMappings(PlatformAnthropic, []ReasoningEffortMapping{{From: "low", To: "high"}})
	require.NoError(t, err)
	_, err = NormalizeReasoningEffortMappings(PlatformOpenAI, []ReasoningEffortMapping{
		{From: "max", To: "xhigh"},
		{From: "MAX", To: "low"},
	})
	require.ErrorContains(t, err, "duplicate")
}

func TestApplyOpenAIReasoningEffortPolicy(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		max      string
		mappings []ReasoningEffortMapping
		path     string
		want     string
		changed  bool
	}{
		{name: "empty policy exact noop", body: ` {"reasoning_effort":"high"} `, path: "reasoning_effort", want: "high"},
		{name: "nested cap", body: `{"reasoning":{"effort":"xhigh"}}`, max: "medium", path: "reasoning.effort", want: "medium", changed: true},
		{name: "flat cap", body: `{"reasoning_effort":"max"}`, max: "xhigh", path: "reasoning_effort", want: "xhigh", changed: true},
		{name: "mapping before cap", body: `{"reasoning_effort":"max"}`, max: "medium", mappings: []ReasoningEffortMapping{{From: "max", To: "xhigh"}}, path: "reasoning_effort", want: "medium", changed: true},
		{name: "mapping is one hop", body: `{"reasoning_effort":"max"}`, mappings: []ReasoningEffortMapping{{From: "max", To: "xhigh"}, {From: "xhigh", To: "low"}}, path: "reasoning_effort", want: "xhigh", changed: true},
		{name: "unknown remains unchanged", body: `{"reasoning_effort":"future"}`, max: "low", path: "reasoning_effort", want: "future"},
		{name: "omitted is not added", body: `{"model":"gpt-5.6"}`, max: "low", path: "reasoning_effort", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(tt.body)
			got, changed := ApplyOpenAIReasoningEffortPolicy(input, tt.max, tt.mappings)
			require.Equal(t, tt.changed, changed)
			require.Equal(t, tt.want, gjson.GetBytes(got, tt.path).String())
			if !tt.changed {
				require.Equal(t, input, got)
			}
		})
	}
}

func TestReasoningEffortMappingsRespectModelScopePrecedence(t *testing.T) {
	mappings, err := NormalizeReasoningEffortMappings(PlatformOpenAI, []ReasoningEffortMapping{
		{From: "high", To: "low"},
		{From: "high", To: "medium", MatchType: domain.ReasoningEffortMatchPrefix, Model: "gpt-5"},
		{From: "high", To: "xhigh", MatchType: domain.ReasoningEffortMatchSuffix, Model: "-codex"},
		{From: "high", To: "max", MatchType: domain.ReasoningEffortMatchExact, Model: "gpt-5-codex"},
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		model string
		want  string
	}{
		{model: "gpt-5-codex", want: "max"},
		{model: "gpt-5-mini", want: "medium"},
		{model: "other-codex", want: "xhigh"},
		{model: "other", want: "low"},
	} {
		body := []byte(`{"model":"` + tc.model + `","reasoning":{"effort":"high"}}`)
		got, changed, policyErr := ApplyOpenAIReasoningEffortPolicyWithAction(body, "", mappings, ReasoningEffortOverLimitDowngrade)
		require.NoError(t, policyErr)
		require.True(t, changed)
		require.Equal(t, tc.want, gjson.GetBytes(got, "reasoning.effort").String())
	}
}

func TestReasoningEffortOverLimitDenyAndRequestedValueContext(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","reasoning":{"effort":"xhigh"}}`)
	got, changed, err := ApplyOpenAIReasoningEffortPolicyWithAction(body, "medium", nil, ReasoningEffortOverLimitDeny)
	require.ErrorAs(t, err, new(*ReasoningEffortOverLimitError))
	require.False(t, changed)
	require.Equal(t, body, got)

	ctx := WithRequestedReasoningEffort(context.Background(), ExtractOpenAIReasoningEffort(body))
	requested := RequestedReasoningEffortFromContext(ctx)
	require.NotNil(t, requested)
	require.Equal(t, "xhigh", *requested)
}

func TestRequestedReasoningEffortContextBoundsUsageLogValue(t *testing.T) {
	ctx := WithRequestedReasoningEffort(context.Background(), "  abcdefghijklmnopqrstuvwxyz  ")
	requested := RequestedReasoningEffortFromContext(ctx)
	require.NotNil(t, requested)
	require.Equal(t, "abcdefghijklmnopqrst", *requested)
}

func TestReasoningEffortPolicyHandlesAnthropicOutputConfigEffort(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","output_config":{"effort":"high"}}`)
	require.Equal(t, "high", ExtractOpenAIReasoningEffort(body))

	got, changed, err := ApplyOpenAIReasoningEffortPolicyWithAction(body, "medium", nil, ReasoningEffortOverLimitDowngrade)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "medium", gjson.GetBytes(got, "output_config.effort").String())

	denied, changed, err := ApplyOpenAIReasoningEffortPolicyWithAction(body, "medium", nil, ReasoningEffortOverLimitDeny)
	require.ErrorAs(t, err, new(*ReasoningEffortOverLimitError))
	require.False(t, changed)
	require.Equal(t, body, denied)
}

func TestReasoningEffortPolicyForWSUsesSessionModelAndKeepsRequestedValue(t *testing.T) {
	group := &Group{
		MaxReasoningEffort:          "medium",
		MaxReasoningEffortOverLimit: ReasoningEffortOverLimitDowngrade,
		ReasoningEffortMappings: []ReasoningEffortMapping{
			{From: "high", To: "low", MatchType: domain.ReasoningEffortMatchPrefix, Model: "gpt-5.6"},
		},
	}
	body := []byte(`{"type":"response.create","reasoning":{"effort":"high"}}`)

	got, requested, err := applyOpenAIReasoningEffortPolicyForWS(body, "gpt-5.6-sol", group)
	require.NoError(t, err)
	require.Equal(t, "high", requested)
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(got, "model").String())
	require.Equal(t, "low", gjson.GetBytes(got, "reasoning.effort").String())
}

func TestReasoningEffortPolicyForWSDeniesOverLimit(t *testing.T) {
	group := &Group{
		MaxReasoningEffort:          "medium",
		MaxReasoningEffortOverLimit: ReasoningEffortOverLimitDeny,
	}
	body := []byte(`{"type":"response.create","model":"gpt-5.6-sol","reasoning":{"effort":"high"}}`)

	got, requested, err := applyOpenAIReasoningEffortPolicyForWS(body, "gpt-5.6-sol", group)
	require.Error(t, err)
	require.Equal(t, "high", requested)
	require.Equal(t, body, got)
}

func TestReasoningEffortPolicyForWSMatchesClientModelWithoutReplacingMappedModel(t *testing.T) {
	group := &Group{
		MaxReasoningEffortOverLimit: ReasoningEffortOverLimitDowngrade,
		ReasoningEffortMappings: []ReasoningEffortMapping{
			{From: "high", To: "low", MatchType: domain.ReasoningEffortMatchExact, Model: "client-alias"},
		},
	}
	body := []byte(`{"type":"response.create","model":"gpt-5.6-sol","reasoning":{"effort":"high"}}`)

	got, requested, err := applyOpenAIReasoningEffortPolicyForWS(body, "client-alias", group)
	require.NoError(t, err)
	require.Equal(t, "high", requested)
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(got, "model").String())
	require.Equal(t, "low", gjson.GetBytes(got, "reasoning.effort").String())
}
