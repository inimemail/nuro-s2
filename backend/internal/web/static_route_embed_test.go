//go:build embed

package web

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestShouldBypassEmbeddedFrontend(t *testing.T) {
	t.Parallel()

	for _, path := range []string{
		"/alpha/search",
		"/alpha/search/",
		"/videos/task",
		"/internal/edge/retry",
		"/internal/runtime/drain",
		"/metrics",
		"/readyz",
	} {
		require.True(t, shouldBypassEmbeddedFrontend(path), path)
	}
	require.False(t, shouldBypassEmbeddedFrontend("/admin/groups"))
	require.False(t, shouldBypassEmbeddedFrontend("/internal/unknown"))
}
