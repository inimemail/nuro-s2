//go:build embed || unit

package web

import (
	"net/http"
	"strings"
)

const staticAssetsCacheControl = "public, max-age=31536000, immutable"

func isLongCacheStaticPath(cleanPath string) bool {
	cleanPath = strings.TrimPrefix(cleanPath, "/")
	if !strings.HasPrefix(cleanPath, "assets/") {
		return false
	}
	name := cleanPath[strings.LastIndex(cleanPath, "/")+1:]
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return false
	}
	stem := name[:dot]
	// Vite's default content hash is eight base64url characters. Checking the
	// exact suffix avoids treating ordinary names such as my-custom-logo.png as
	// immutable while still accepting hashes that themselves contain '-'.
	const viteHashLength = 8
	dash := len(stem) - viteHashLength - 1
	if dash <= 0 || stem[dash] != '-' {
		return false
	}
	for _, char := range stem[dash+1:] {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func applyStaticAssetCacheHeaders(header http.Header, cleanPath string) {
	if header == nil || !isLongCacheStaticPath(cleanPath) {
		return
	}
	header.Set("Cache-Control", staticAssetsCacheControl)
}
