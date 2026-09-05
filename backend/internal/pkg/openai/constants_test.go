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
