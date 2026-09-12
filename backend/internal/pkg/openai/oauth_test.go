package openai

import (
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestSessionStore_Stop_Idempotent(t *testing.T) {
	store := NewSessionStore()

	store.Stop()
	store.Stop()

	select {
	case <-store.stopCh:
		// ok
	case <-time.After(time.Second):
		t.Fatal("stopCh 未关闭")
	}
}

func TestSessionStore_Stop_Concurrent(t *testing.T) {
	store := NewSessionStore()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Stop()
		}()
	}

	wg.Wait()

	select {
	case <-store.stopCh:
		// ok
	case <-time.After(time.Second):
		t.Fatal("stopCh 未关闭")
	}
}

func TestBuildAuthorizationURLForPlatform_OpenAI(t *testing.T) {
	authURL := BuildAuthorizationURLForPlatform("state-1", "challenge-1", DefaultRedirectURI, OAuthPlatformOpenAI)
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("Parse URL failed: %v", err)
	}
	q := parsed.Query()
	if got := q.Get("client_id"); got != ClientID {
		t.Fatalf("client_id mismatch: got=%q want=%q", got, ClientID)
	}
	if got := q.Get("codex_cli_simplified_flow"); got != "true" {
		t.Fatalf("codex flow mismatch: got=%q want=true", got)
	}
	if got := q.Get("id_token_add_organizations"); got != "true" {
		t.Fatalf("id_token_add_organizations mismatch: got=%q want=true", got)
	}
	if got := q.Get("originator"); got != DefaultOriginator {
		t.Fatalf("originator mismatch: got=%q want=%q", got, DefaultOriginator)
	}
	if got := q.Get("scope"); got != DefaultScopes {
		t.Fatalf("scope mismatch: got=%q want=%q", got, DefaultScopes)
	}
}

func TestOAuthTokenFormsIncludeCompatibleScopes(t *testing.T) {
	authCodeForm, err := url.ParseQuery(BuildTokenRequest("code", "verifier", "").ToFormData())
	if err != nil {
		t.Fatalf("parse authorization-code form: %v", err)
	}
	if got := authCodeForm.Get("scope"); got != DefaultScopes {
		t.Fatalf("authorization-code scope = %q, want %q", got, DefaultScopes)
	}

	refreshForm, err := url.ParseQuery(BuildRefreshTokenRequest("refresh").ToFormData())
	if err != nil {
		t.Fatalf("parse refresh form: %v", err)
	}
	if got := refreshForm.Get("scope"); got != RefreshScopes {
		t.Fatalf("refresh scope = %q, want %q", got, RefreshScopes)
	}
}
