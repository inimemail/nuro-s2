package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractGrokWebSearchSourcesNormalizesAndDeduplicates(t *testing.T) {
	body := []byte(`{"output":[{"type":"web_search_call","action":{"sources":[{"url":"HTTPS://Example.com/a#frag","title":"Example"},{"url":"https://example.com/a","title":"Duplicate"},{"url":"javascript:bad"}]}},{"type":"message","content":[{"type":"output_text","annotations":[{"type":"url_citation","url":"https://example.org/x","title":"Org"}]}]}]}`)
	got := extractGrokWebSearchSources(body, 5)
	require.Len(t, got, 2)
	require.Equal(t, "HTTPS://Example.com/a#frag", got[0].URL)
	require.Equal(t, "Example", got[0].Title)
	require.Equal(t, "example.org/x", got[1].URL[len("https://"):])
}

func TestNormalizeGrokWebSearchMaxResults(t *testing.T) {
	require.Equal(t, 5, normalizeGrokWebSearchMaxResults(0))
	require.Equal(t, 20, normalizeGrokWebSearchMaxResults(100))
}
