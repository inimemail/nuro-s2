package openai

import (
	"net/http"
	"testing"
)

func TestEvaluateEngineFingerprint(t *testing.T) {
	header := http.Header{}
	header.Set("X-Codex-Installation-Id", "install-1")
	body := []byte(`{"client_metadata":{"x-codex-window-id":"win-1"}}`)

	if !EvaluateEngineFingerprint(header, body, []EngineFingerprintSignal{
		{Type: FingerprintSignalHeaderPrefix, Match: []string{"x-codex-"}, Required: true},
		{Type: FingerprintSignalBodyPath, Match: []string{"client_metadata.x-codex-window-id"}, Required: true},
	}) {
		t.Fatal("required header prefix and body path should match")
	}

	if EvaluateEngineFingerprint(header, body, []EngineFingerprintSignal{
		{Type: FingerprintSignalHeaderExact, Match: []string{"missing-header"}, Required: true},
	}) {
		t.Fatal("missing required signal should fail")
	}

	if !EvaluateEngineFingerprint(nil, nil, []EngineFingerprintSignal{
		{Type: FingerprintSignalHeaderExact, Match: []string{"missing-header"}, Required: false},
	}) {
		t.Fatal("optional missing signal should not fail")
	}
}

func TestValidateEngineFingerprintSignalsJSON(t *testing.T) {
	if err := ValidateEngineFingerprintSignalsJSON(`[{"type":"header_prefix","match":["x-codex-"],"required":true}]`); err != nil {
		t.Fatalf("valid fingerprint signals rejected: %v", err)
	}
	if err := ValidateEngineFingerprintSignalsJSON(`[{"type":"bad","match":["x"],"required":true}]`); err == nil {
		t.Fatal("invalid signal type should be rejected")
	}
	if err := ValidateEngineFingerprintSignalsJSON(`[{"type":"header_exact","match":[" "],"required":true}]`); err == nil {
		t.Fatal("blank-only match list should be rejected")
	}
}
