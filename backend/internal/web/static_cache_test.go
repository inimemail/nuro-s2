//go:build embed || unit

package web

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyStaticAssetCacheHeaders(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/assets/index-abc12345.js", "assets/app-abc12345.css", "/assets/KeyUsageView-DCx0Dm-S.js"} {
		header := make(http.Header)
		applyStaticAssetCacheHeaders(header, path)
		require.Equal(t, staticAssetsCacheControl, header.Get("Cache-Control"), path)
	}

	for _, path := range []string{"/", "/index.html", "/logo.png", "/favicon.ico", "/assets/app.css", "/assets/my-custom-logo.png", "/admin/groups", "/alpha/search", "/videos/task", "/internal/edge/retry"} {
		header := make(http.Header)
		applyStaticAssetCacheHeaders(header, path)
		require.Empty(t, header.Get("Cache-Control"), path)
	}
}
