package securityaudit

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

type closeTrackingAuditClient struct {
	response *http.Response
	err      error
	closed   bool
}

func (c *closeTrackingAuditClient) Do(*http.Request) (*http.Response, error) {
	return c.response, c.err
}

func (c *closeTrackingAuditClient) CloseIdleConnections() {
	c.closed = true
}

func TestParseGuardResultStrictAndConservative(t *testing.T) {
	t.Run("safe", func(t *testing.T) {
		got, err := parseGuardResult("Safety: Safe\nCategories: None", defaultScanners)
		if err != nil || got.Decision != "pass" || got.Risk != "low" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})

	t.Run("elevated controversial category", func(t *testing.T) {
		got, err := parseGuardResult("Categories: PII\nSafety: Controversial", defaultScanners)
		if err != nil || got.Decision != "critical" || got.Risk != "critical" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})

	t.Run("unknown unsafe category is hashed", func(t *testing.T) {
		got, err := parseGuardResult("Safety: Unsafe\nCategories: vendor-private-label", defaultScanners)
		if err != nil || got.Decision != "critical" || len(got.Categories) != 1 {
			t.Fatalf("got=%+v err=%v", got, err)
		}
		if !strings.HasPrefix(got.Categories[0], "unknown:") || strings.Contains(got.Categories[0], "vendor") {
			t.Fatalf("unknown category was not safely hashed: %q", got.Categories[0])
		}
	})

	t.Run("safe cannot override critical category", func(t *testing.T) {
		got, err := parseGuardResult("Safety: Safe\nCategories: jailbreak", defaultScanners)
		if err != nil || got.Decision != "critical" || got.Action != "Block" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})

	t.Run("unsafe remains critical when category is disabled", func(t *testing.T) {
		got, err := parseGuardResult("Safety: Unsafe\nCategories: pii", []string{"violent"})
		if err != nil || got.Decision != "critical" || got.Risk != "critical" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})

	t.Run("critical category remains active when scanner is disabled", func(t *testing.T) {
		got, err := parseGuardResult("Safety: Safe\nCategories: pii", []string{"violent"})
		if err != nil || got.Decision != "critical" || got.Risk != "critical" || got.Action != "Block" {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})

	for _, invalid := range []string{
		"Safety: Safe",
		"Safe\nNone",
		"Safety: Maybe\nCategories: None",
		"Safety: Safe\nCategories: None\nextra",
		"Safety: Safe\nSafety: Unsafe\nCategories: None",
		"Safety: Safe\nCategories: None\nCategories: pii",
	} {
		if _, err := parseGuardResult(invalid, defaultScanners); err == nil {
			t.Fatalf("expected strict parse failure for %q", invalid)
		}
	}
}

func TestNormalizeEndpointURL(t *testing.T) {
	tests := []struct {
		name    string
		input   EndpointConfig
		want    string
		wantErr bool
	}{
		{name: "root", input: EndpointConfig{BaseURL: "https://guard.example"}, want: "https://guard.example/v1/chat/completions"},
		{name: "v1", input: EndpointConfig{BaseURL: "https://guard.example/v1/"}, want: "https://guard.example/v1/chat/completions"},
		{name: "private http explicit", input: EndpointConfig{BaseURL: "http://127.0.0.1:8080", AllowPrivate: true}, want: "http://127.0.0.1:8080/v1/chat/completions"},
		{name: "public http rejected", input: EndpointConfig{BaseURL: "http://guard.example"}, wantErr: true},
		{name: "credentials rejected", input: EndpointConfig{BaseURL: "https://user:secret@guard.example"}, wantErr: true},
		{name: "query rejected", input: EndpointConfig{BaseURL: "https://guard.example?token=secret"}, wantErr: true},
		{name: "non http rejected", input: EndpointConfig{BaseURL: "file:///tmp/guard"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeEndpointURL(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got=%q want=%q err=%v", got, tt.want, err)
			}
		})
	}
}

func TestAuditDestinationPolicy(t *testing.T) {
	for _, raw := range []string{"0.0.0.0", "169.254.169.254", "224.0.0.1", "::", "fe80::1", "ff02::1"} {
		if !forbiddenAuditIP(netip.MustParseAddr(raw)) {
			t.Fatalf("expected %s to be forbidden", raw)
		}
	}
	prefixes, err := parseAllowedCIDRs([]string{"127.0.0.0/8", "10.0.0.0/24"})
	if err != nil || !ipAllowed(netip.MustParseAddr("127.0.0.1"), prefixes) || ipAllowed(netip.MustParseAddr("10.0.1.1"), prefixes) {
		t.Fatalf("CIDR policy mismatch: prefixes=%v err=%v", prefixes, err)
	}
	if _, err := parseAllowedCIDRs([]string{"not-a-cidr"}); err == nil {
		t.Fatal("expected invalid CIDR error")
	}
	private := EndpointConfig{AllowPrivate: true}
	if !auditDestinationAllowed(private, "http", netip.MustParseAddr("127.0.0.1"), prefixes) {
		t.Fatal("explicitly allowlisted private HTTP destination was rejected")
	}
	if auditDestinationAllowed(private, "http", netip.MustParseAddr("1.1.1.1"), prefixes) {
		t.Fatal("AllowPrivate must not permit cleartext public HTTP")
	}
	if !auditDestinationAllowed(EndpointConfig{}, "https", netip.MustParseAddr("1.1.1.1"), nil) {
		t.Fatal("public HTTPS destination was rejected")
	}
}

func TestEndpointCredentialBindingRequiresSameNormalizedURL(t *testing.T) {
	configured := EndpointConfig{BaseURL: "https://Guard.Example/v1/"}
	if !endpointCredentialBindingEqual(configured, EndpointConfig{BaseURL: "https://guard.example"}) {
		t.Fatal("equivalent normalized endpoint should preserve credentials")
	}
	for _, candidate := range []EndpointConfig{
		{BaseURL: "https://other.example"},
		{BaseURL: "https://guard.example:8443"},
		{BaseURL: "https://guard.example/custom"},
		{BaseURL: "http://guard.example", AllowPrivate: true},
	} {
		if endpointCredentialBindingEqual(configured, candidate) {
			t.Fatalf("changed endpoint unexpectedly preserved credentials: %q", candidate.BaseURL)
		}
	}
}

func TestScanEndpointClosesIdleConnections(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *http.Response
		err      error
		wantErr  bool
	}{
		{
			name: "success",
			response: &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(
					`{"choices":[{"message":{"content":"Safety: Safe\nCategories: None"}}]}`,
				)),
			},
		},
		{name: "request failure", err: errors.New("dial failed"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &closeTrackingAuditClient{response: test.response, err: test.err}
			_, err := (&Service{}).scanEndpointWithClient(
				context.Background(),
				EndpointConfig{ID: "guard", Model: "qwen3guard", TimeoutMS: 1000},
				defaultScanners,
				"hello",
				client,
				"https://guard.example/v1/chat/completions",
			)
			if (err != nil) != test.wantErr {
				t.Fatalf("scanEndpointWithClient() err=%v wantErr=%v", err, test.wantErr)
			}
			if !client.closed {
				t.Fatal("scan did not close idle connections")
			}
		})
	}
}

func TestSecureEndpointClientBlocksRedirectAndUnlistedPrivateIP(t *testing.T) {
	redirectTargetCalled := false
	newIPv4Server := func(handler http.Handler) *httptest.Server {
		server := httptest.NewUnstartedServer(handler)
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server.Listener = listener
		server.Start()
		return server
	}
	target := newIPv4Server(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetCalled = true
	}))
	defer target.Close()
	redirect := newIPv4Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	client, endpointURL, err := secureEndpointClient(EndpointConfig{
		BaseURL: redirect.URL, TimeoutMS: 1000, AllowPrivate: true, AllowedCIDRs: []string{"127.0.0.0/8", "::1/128"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, endpointURL, strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || redirectTargetCalled {
		t.Fatalf("redirect followed: status=%d target_called=%v", resp.StatusCode, redirectTargetCalled)
	}

	blocked, _, err := secureEndpointClient(EndpointConfig{BaseURL: redirect.URL, TimeoutMS: 500, AllowPrivate: true, AllowedCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	transport := blocked.Transport.(*http.Transport)
	address := strings.TrimPrefix(redirect.URL, "http://")
	conn, dialErr := transport.DialContext(context.Background(), "tcp", address)
	if conn != nil {
		_ = conn.Close()
	}
	if dialErr == nil {
		t.Fatal("expected unlisted loopback destination to be blocked")
	}
}

func TestForbiddenAuditIPAcceptsPublicAddress(t *testing.T) {
	if forbiddenAuditIP(netip.MustParseAddr("1.1.1.1")) {
		t.Fatal("public address unexpectedly forbidden")
	}
	if _, err := net.ResolveTCPAddr("tcp", "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
}
