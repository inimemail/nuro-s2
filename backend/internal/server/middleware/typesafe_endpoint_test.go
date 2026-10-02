package middleware

import "testing"

func TestTypeSafeEndpointAllowed(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/v1/systemone", want: true},
		{path: "/v1/models", want: true},
		{path: "/v1/models/jev-latest", want: true},
		{path: "/models", want: true},
		{path: "/models/jev-latest", want: true},
		{path: "/v1/usage", want: true},
		{path: "/usage", want: true},
		{path: "/v1/sub2api/billing", want: true},
		{path: "/v1/messages", want: false},
		{path: "/v1/responses", want: false},
		{path: "/v1/chat/completions", want: false},
		{path: "/v1/systemone/extra", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			if got := typeSafeEndpointAllowed(tc.path); got != tc.want {
				t.Fatalf("typeSafeEndpointAllowed(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
