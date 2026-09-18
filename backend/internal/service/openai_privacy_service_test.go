package service

import "testing"

func TestIsCloudflareChallengeResponse(t *testing.T) {
	tests := []struct {
		name        string
		cfMitigated string
		body        string
		want        bool
	}{
		{name: "header", cfMitigated: "challenge", want: true},
		{name: "header case insensitive", cfMitigated: " Challenge ", want: true},
		{name: "challenge platform", body: "<script src=\"/cdn-cgi/challenge-platform/h/b/scripts/jsd/main.js\"></script>", want: true},
		{name: "cloudflare html", body: "Just a Moment...", want: true},
		{name: "ordinary error", body: `{"error":"temporarily unavailable"}`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCloudflareChallengeResponse(tt.cfMitigated, tt.body); got != tt.want {
				t.Fatalf("isCloudflareChallengeResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}
