package openai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultModelsContainsGPT56Catalog(t *testing.T) {
	byID := make(map[string]Model, len(DefaultModels))
	for _, model := range DefaultModels {
		byID[model.ID] = model
	}

	for _, tc := range []struct {
		id          string
		displayName string
	}{
		{id: "gpt-5.6", displayName: "GPT-5.6 (Sol)"},
		{id: "gpt-5.6-sol", displayName: "GPT-5.6 Sol"},
		{id: "gpt-5.6-terra", displayName: "GPT-5.6 Terra"},
		{id: "gpt-5.6-luna", displayName: "GPT-5.6 Luna"},
	} {
		model, ok := byID[tc.id]
		require.True(t, ok, "expected %s in DefaultModels", tc.id)
		require.Equal(t, int64(1780876800), model.Created)
		require.Equal(t, tc.displayName, model.DisplayName)
		require.Equal(t, "openai", model.OwnedBy)
	}
	require.NotContains(t, byID, "gpt-5.5-pro")
	require.Equal(t, "gpt-5.6", DefaultModels[0].ID, "preserve the local account-test default")

	for _, tc := range []struct {
		id          string
		displayName string
	}{
		{id: "gpt-6-astra", displayName: "GPT-6 Astra"},
		{id: "gpt-6", displayName: "GPT-6 (Astra)"},
	} {
		model, ok := byID[tc.id]
		require.True(t, ok, "expected %s in DefaultModels", tc.id)
		require.Equal(t, int64(1788480000), model.Created)
		require.Equal(t, tc.displayName, model.DisplayName)
		require.Equal(t, "openai", model.OwnedBy)
	}
}

func TestGPT61SolModelSpelling(t *testing.T) {
	for _, model := range []string{
		"gpt-6.1-sol",
		"gpt_6.1_sol-high",
		"openai/gpt-6.1-sol-high",
		"models/gpt-6.1-sol-openai-compact",
		"gpt-6.1-sol-none",
	} {
		require.True(t, IsGPT61SolModelSpelling(model), model)
	}
	for _, model := range []string{"gpt-6.1-solar", "gpt-6.1-sol-foo"} {
		require.False(t, IsGPT61SolModelSpelling(model), model)
	}
}

func TestGPT61SolReasoningEffortValidation(t *testing.T) {
	for _, effort := range []string{"none", "minimal", " NONE "} {
		require.Error(t, ValidateGPT61SolReasoningEffort("openai/gpt-6.1-sol", effort), effort)
	}
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max", ""} {
		require.NoError(t, ValidateGPT61SolReasoningEffort("gpt-6.1-sol", effort), effort)
	}
	require.NoError(t, ValidateGPT61SolReasoningEffort("gpt-6-sol", "none"))
}

func TestGPT61SolModelRequestValidationRejectsUnsupportedAliases(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol-none", "gpt_6.1_sol-minimal", "openai/gpt-6.1-sol-none"} {
		require.Error(t, ValidateGPT61SolModelRequest(model, ""), model)
	}
	for _, model := range []string{"gpt-6.1-sol", "gpt-6.1-sol-high", "gpt-6.1-sol-max"} {
		require.NoError(t, ValidateGPT61SolModelRequest(model, ""), model)
	}
	require.Equal(t, "high", GPT61SolModelAliasReasoningEffort("models/gpt-6.1-sol-high"))
	require.Empty(t, GPT61SolModelAliasReasoningEffort("gpt-6.1-sol-openai-compact"))
}
