package xai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultModelsIncludesGrok45AndComposer(t *testing.T) {
	ids := DefaultModelIDs()

	require.Contains(t, ids, "grok-4.5")
	require.Contains(t, ids, "grok-composer-2.5-fast")
}

func TestDefaultModelMappingUsesGrok45(t *testing.T) {
	mapping := DefaultModelMapping()

	require.Equal(t, "grok-4.5", mapping["grok"])
	require.Equal(t, "grok-4.5", mapping["grok-latest"])
	require.Equal(t, "grok-4.5", mapping["grok-4.5-latest"])
	require.Equal(t, "grok-4.5", mapping["grok-build-latest"])
	require.Equal(t, "grok-composer-2.5-fast", mapping["grok-composer"])
}
