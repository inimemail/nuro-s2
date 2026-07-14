package responseheaders

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestFilterHeadersDisabledUsesDefaultAllowlist(t *testing.T) {
	src := http.Header{}
	src.Add("Content-Type", "application/json")
	src.Add("X-Request-Id", "req-123")
	src.Add("X-Test", "ok")
	src.Add("Connection", "keep-alive")
	src.Add("Content-Length", "123")

	cfg := config.ResponseHeaderConfig{
		Enabled:     false,
		ForceRemove: []string{"x-request-id"},
	}

	filtered := FilterHeaders(src, CompileHeaderFilter(cfg))
	if filtered.Get("Content-Type") != "application/json" {
		t.Fatalf("expected Content-Type passthrough, got %q", filtered.Get("Content-Type"))
	}
	if filtered.Get("X-Request-Id") != PublicRequestID("req-123") {
		t.Fatalf("expected opaque X-Request-Id, got %q", filtered.Get("X-Request-Id"))
	}
	if filtered.Get("X-Test") != "" {
		t.Fatalf("expected X-Test removed, got %q", filtered.Get("X-Test"))
	}
	if filtered.Get("Connection") != "" {
		t.Fatalf("expected Connection to be removed, got %q", filtered.Get("Connection"))
	}
	if filtered.Get("Content-Length") != "" {
		t.Fatalf("expected Content-Length to be removed, got %q", filtered.Get("Content-Length"))
	}
}

func TestFilterHeadersEnabledUsesAllowlist(t *testing.T) {
	src := http.Header{}
	src.Add("Content-Type", "application/json")
	src.Add("X-Extra", "ok")
	src.Add("X-Remove", "nope")
	src.Add("X-Blocked", "nope")

	cfg := config.ResponseHeaderConfig{
		Enabled:           true,
		AdditionalAllowed: []string{"x-extra"},
		ForceRemove:       []string{"x-remove"},
	}

	filtered := FilterHeaders(src, CompileHeaderFilter(cfg))
	if filtered.Get("Content-Type") != "application/json" {
		t.Fatalf("expected Content-Type allowed, got %q", filtered.Get("Content-Type"))
	}
	if filtered.Get("X-Extra") != "ok" {
		t.Fatalf("expected X-Extra allowed, got %q", filtered.Get("X-Extra"))
	}
	if filtered.Get("X-Remove") != "" {
		t.Fatalf("expected X-Remove removed, got %q", filtered.Get("X-Remove"))
	}
	if filtered.Get("X-Blocked") != "" {
		t.Fatalf("expected X-Blocked removed, got %q", filtered.Get("X-Blocked"))
	}
}

func TestFilterHeadersNeverAllowsUpstreamIdentityHeaders(t *testing.T) {
	src := http.Header{
		"Location":         []string{"https://private-upstream.example/error"},
		"Www-Authenticate": []string{`Bearer realm="private-upstream.example"`},
		"Server":           []string{"private-provider-edge"},
		"Via":              []string{"private-provider.example"},
		"X-Powered-By":     []string{"private-provider"},
		"Cf-Ray":           []string{"provider-trace"},
		"X-Openai-Version": []string{"2026-07-01"},
		"X-Amzn-Trace-Id":  []string{"Root=trace"},
		"X-Extra":          []string{"https://private-upstream.example/error"},
		"X-Provider-Name":  []string{"OpenRouter"},
	}
	filter := CompileHeaderFilter(config.ResponseHeaderConfig{
		Enabled:           true,
		AdditionalAllowed: []string{"location", "www-authenticate", "server", "via", "x-powered-by", "cf-ray", "x-openai-version", "x-amzn-trace-id", "x-extra", "x-provider-name"},
	})

	filtered := FilterHeaders(src, filter)
	if filtered.Get("Location") != "" || filtered.Get("Www-Authenticate") != "" ||
		filtered.Get("Server") != "" || filtered.Get("Via") != "" ||
		filtered.Get("X-Powered-By") != "" || filtered.Get("Cf-Ray") != "" ||
		filtered.Get("X-Openai-Version") != "" || filtered.Get("X-Amzn-Trace-Id") != "" ||
		filtered.Get("X-Extra") != "" || filtered.Get("X-Provider-Name") != "" {
		t.Fatalf("sensitive upstream headers must remain blocked: %#v", filtered)
	}
}

func TestSafeContentTypeRemovesUpstreamIdentityAndExtensionParameters(t *testing.T) {
	if got := SafeContentType(`application/json; profile="https://private-upstream.example/schema"`, "application/json"); got != "application/json" {
		t.Fatalf("expected sensitive profile to use fallback, got %q", got)
	}
	if got := SafeContentType(`application/json; vendor="private-provider"; charset=utf-8`, "application/octet-stream"); got != "application/json; charset=utf-8" {
		t.Fatalf("expected extension parameter removal, got %q", got)
	}
	if got := SafeContentType(`multipart/mixed; boundary=batch_123`, "application/octet-stream"); got != "multipart/mixed; boundary=batch_123" {
		t.Fatalf("expected multipart boundary preservation, got %q", got)
	}
}

func TestFilterHeadersSanitizesContentTypeBeforeForwarding(t *testing.T) {
	src := http.Header{
		"Content-Type": []string{`application/json; vendor="private-provider"; charset=utf-8`},
	}
	filtered := FilterHeaders(src, nil)
	if got := filtered.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("expected sanitized Content-Type, got %q", got)
	}
}

func TestFilterHeadersMakesUpstreamRequestIDOpaque(t *testing.T) {
	src := http.Header{"X-Request-Id": []string{"openai-private-provider.example/request/123"}}
	filtered := FilterHeaders(src, nil)
	got := filtered.Get("X-Request-Id")
	if got != PublicRequestID(src.Get("X-Request-Id")) {
		t.Fatalf("expected stable opaque request ID, got %q", got)
	}
	if got == src.Get("X-Request-Id") {
		t.Fatal("upstream request ID must not be forwarded verbatim")
	}
}
