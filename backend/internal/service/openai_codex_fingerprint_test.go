package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestCodexFingerprintHeaderAndBodyShareIDs(t *testing.T) {
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexFingerprintModeExtraKey: string(codexFingerprintFull),
			codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
		},
	}
	ids := resolveCodexFingerprintIDsFromRequest(account, http.Header{"Session-Id": []string{"client-session"}})
	if ids == nil {
		t.Fatal("expected converged IDs")
	}

	headers := make(http.Header)
	headers.Set("x-codex-turn-metadata", `{"turn_id":"client-turn","session_id":"client-session"}`)
	applyCodexFingerprintHeaders(headers, ids)
	body := map[string]any{"client_metadata": map[string]any{"session_id": "client-session"}}
	if !applyCodexFingerprintClientMetadata(body, ids) {
		t.Fatal("expected body to be rewritten")
	}
	metadata := body["client_metadata"].(map[string]any)
	if got := metadata["session_id"]; got != ids.sessionID {
		t.Fatalf("body session_id = %v, want %s", got, ids.sessionID)
	}
	if got := metadata["turn_id"]; got != ids.turnID {
		t.Fatalf("body turn_id = %v, want %s", got, ids.turnID)
	}
	if got := headers.Get("session_id"); got != ids.sessionID {
		t.Fatalf("header session_id = %q, want %s", got, ids.sessionID)
	}
	metadataTurn := map[string]any{}
	if err := json.Unmarshal([]byte(headers.Get("x-codex-turn-metadata")), &metadataTurn); err != nil {
		t.Fatalf("decode rewritten turn metadata: %v", err)
	}
	if metadataTurn["turn_id"] != ids.turnID {
		t.Fatalf("header turn_id = %v, want %s", metadataTurn["turn_id"], ids.turnID)
	}
}

func TestCodexFingerprintPreservesStringMetadata(t *testing.T) {
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintSession),
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	}}
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	body := map[string]any{"client_metadata": map[string]string{"originator": "codex_cli_rs", "session_id": "client"}}
	if !applyCodexFingerprintClientMetadata(body, ids) {
		t.Fatal("expected body to be rewritten")
	}
	metadata, ok := body["client_metadata"].(map[string]any)
	if !ok {
		t.Fatalf("rewritten metadata type = %T, want map[string]any", body["client_metadata"])
	}
	if metadata["originator"] != "codex_cli_rs" {
		t.Fatalf("originator metadata was lost: %#v", metadata)
	}
}

func TestCodexFingerprintStagingRejectsPreviousAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	first := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintSession),
		codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
	}}
	second := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintSession),
		codexFingerprintSeedExtraKey: "22222222-2222-4222-8222-222222222222",
	}}
	stageCodexFingerprintIDs(c, resolveCodexFingerprintIDsFromRequest(first, nil))
	if got := stagedCodexFingerprintIDs(c, second); got != nil {
		t.Fatal("previous account fingerprint leaked into replacement account")
	}
}

func TestOpenAICodexSnapshotStaleForPause(t *testing.T) {
	now := time.Now()
	if !openAICodexSnapshotStaleForPause(map[string]any{"codex_5h_used_percent": 99}, now) {
		t.Fatal("snapshot without update timestamp must not drive auto-pause")
	}
	if !openAICodexSnapshotStaleForPause(map[string]any{"codex_usage_updated_at": "invalid"}, now) {
		t.Fatal("snapshot with invalid update timestamp must not drive auto-pause")
	}
	if !openAICodexSnapshotStaleForPause(map[string]any{
		"codex_usage_updated_at": now.Add(-openAICodexAutoPauseStaleAfter - time.Minute).Format(time.RFC3339),
	}, now) {
		t.Fatal("expected old snapshot to be stale")
	}
	if openAICodexSnapshotStaleForPause(map[string]any{
		"codex_usage_updated_at": now.Add(-time.Minute).Format(time.RFC3339),
	}, now) {
		t.Fatal("fresh snapshot must remain eligible for pause evaluation")
	}
}
