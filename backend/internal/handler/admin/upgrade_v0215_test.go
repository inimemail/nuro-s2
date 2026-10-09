package admin

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestV0215AntigravityModelsHonorPublicMappingKeys(t *testing.T) {
	defaults := antigravity.DefaultModels()
	require.Equal(t, defaults, antigravityAccountTestModels(nil))
	known := defaults[0]
	models := antigravityAccountTestModels(map[string]any{"public-alias": "upstream", known.ID: "other", " ": "invalid"})
	require.Len(t, models, 2)
	byID := make(map[string]antigravity.ClaudeModel)
	for _, model := range models {
		byID[model.ID] = model
	}
	require.Equal(t, known, byID[known.ID])
	require.Equal(t, "public-alias", byID["public-alias"].DisplayName)
	require.Equal(t, "model", byID["public-alias"].Type)
	require.NotContains(t, byID, "upstream")
	require.Equal(t, models, antigravityAccountTestModels(map[string]string{"public-alias": "upstream", known.ID: "other"}))
}
