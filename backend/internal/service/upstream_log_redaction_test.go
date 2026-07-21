package service

import (
	"strings"
	"testing"
)

func TestTruncateForLogRedactsSecretsAndUpstreamIdentity(t *testing.T) {
	secret := `Authorization=Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature api_key=sk-proj-1234567890abcdef`
	got := truncateForLog([]byte(secret), 2048)
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiJ9") || strings.Contains(got, "sk-proj-1234567890abcdef") {
		t.Fatalf("log body contains credential material: %q", got)
	}

	got = truncateForLog([]byte(`<!doctype html><title>provider.example</title>`), 2048)
	if got != safeUpstreamErrorMessage {
		t.Fatalf("expected generic upstream log message, got %q", got)
	}

	got = truncateForLog([]byte(`Authorization=Basic x Cookie=session=y`), 2048)
	if strings.Contains(got, "Basic x") || strings.Contains(got, "session=y") {
		t.Fatalf("short/header-shaped credentials leaked: %q", got)
	}
}
