package openai

import "testing"

func TestPairCodexClientIdentityRejectsControlBeforeTrim(t *testing.T) {
	for _, ua := range []string{"\rcodex_cli_rs/1.0.0", "codex_cli_rs/1.0.0\n", "codex_cli_rs/1.0.0\x00", "codex_cli_rs/1.0.0\r\n x"} {
		if _, _, ok := PairCodexClientIdentity(ua); ok {
			t.Errorf("accepted invalid header %q", ua)
		}
	}
	if _, _, ok := PairCodexClientIdentity(" codex_cli_rs/1.0.0 (Linux) "); !ok {
		t.Fatal("valid identity should still be accepted")
	}
}
