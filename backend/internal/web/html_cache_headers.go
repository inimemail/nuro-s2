//go:build embed

package web

import "net/http"

// Dynamic HTML contains settings and a per-request CSP nonce.
func applyHTMLNoCacheHeaders(h http.Header) {
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
}
