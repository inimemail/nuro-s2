package anthropicfp

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeDatelineScopesSystemAndSystemReminder(t *testing.T) {
	body := []byte(`{
		"system": "Today’s date is 2026/07/01.",
		"messages": [
			{"role": "user", "content": "Today’s date is 2026/07/01."},
			{"role": "user", "content": "<system-reminder>Todayʼs date is 2026/07/01.</system-reminder>"}
		]
	}`)

	normalized, hits, changed := NormalizeDateline(body)
	if !changed {
		t.Fatal("expected dateline normalization to change scoped text")
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if got := gjson.GetBytes(normalized, "system").String(); got != "Today's date is 2026-07-01." {
		t.Fatalf("system = %q", got)
	}
	if got := gjson.GetBytes(normalized, "messages.0.content").String(); got != "Today’s date is 2026/07/01." {
		t.Fatalf("user content should be unchanged, got %q", got)
	}
	if got := gjson.GetBytes(normalized, "messages.1.content").String(); got != "<system-reminder>Today's date is 2026-07-01.</system-reminder>" {
		t.Fatalf("system reminder = %q", got)
	}
}

func TestNormalizeDatelineNoopForCanonicalASCII(t *testing.T) {
	body := []byte(`{"system":"Today's date is 2026-07-01."}`)

	normalized, hits, changed := NormalizeDateline(body)
	if changed {
		t.Fatal("canonical ASCII dateline should not be rewritten")
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %d, want 0", len(hits))
	}
	if string(normalized) != string(body) {
		t.Fatalf("body changed: %s", normalized)
	}
}
