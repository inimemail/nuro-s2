package service

import (
	"testing"

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
	require.Error(t, err)
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
