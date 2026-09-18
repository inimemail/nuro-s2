package xai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultModelsIncludesGrok45AndComposer(t *testing.T) {
	ids := DefaultModelIDs()

	require.Contains(t, ids, "grok-4.5")
	require.Contains(t, ids, "grok-composer-2.5-fast")
	require.Contains(t, ids, DefaultImagineImage20Model)
	require.NotContains(t, ids, "grok-imagine-edit")
	require.NotContains(t, ids, "grok-imagine-video-1.5-preview")
}

func TestDefaultModelMappingUsesGrok45(t *testing.T) {
	mapping := DefaultModelMapping()

	require.Equal(t, "grok-4.5", mapping["grok"])
	require.Equal(t, "grok-4.5", mapping["grok-latest"])
	require.Equal(t, "grok-4.5", mapping["grok-4.5-latest"])
	require.Equal(t, "grok-build-0.1", mapping["grok-build-latest"])
	require.Equal(t, DefaultImagineImage20Model, mapping[DefaultImagineImage20Model])
	for _, legacy := range []string{"grok-imagine-edit", "grok-imagine", "grok-imagine-video-1.5-preview", "grok-3-mini", "grok-3-mini-fast"} {
		require.Equal(t, legacy, mapping[legacy])
		require.Equal(t, legacy, mapping["xai/"+legacy])
	}
	require.Equal(t, "grok-composer-2.5-fast", mapping["grok-composer"])
}
