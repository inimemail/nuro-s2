package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractGrokXSearchSources(t *testing.T) {
	body := []byte(`{"output":[{"type":"x_search_call","action":{"sources":[{"url":"https://x.com/user/status/1","title":"post"}]}}]}`)
	got := extractGrokWebSearchSources(body, 5)
	require.Len(t, got, 1)
	require.Equal(t, "https://x.com/user/status/1", got[0].URL)
}
