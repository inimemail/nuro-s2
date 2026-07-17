//go:build unit

package xai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeKnownBaseURLPathRejectsUnsafeComponents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "userinfo", raw: "https://user:pass@api.x.ai/v1"},
		{name: "query", raw: "https://api.x.ai/v1?target=private"},
		{name: "force query", raw: "https://api.x.ai/v1?"},
		{name: "fragment", raw: "https://api.x.ai/v1#internal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := normalizeKnownBaseURLPath(tt.raw)
			require.Error(t, err)
		})
	}
}

func TestNormalizeKnownBaseURLPathAcceptsKnownShape(t *testing.T) {
	t.Parallel()

	got, err := normalizeKnownBaseURLPath("https://api.x.ai/")
	require.NoError(t, err)
	require.Equal(t, "https://api.x.ai/v1", got)
}

func TestOfficialBaseURLHostsIncludeRegionalAPIEndpoints(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"api.x.ai", "us-east-1.api.x.ai", "us-west-2.api.x.ai", "eu-west-1.api.x.ai"} {
		validated, err := ValidateTrustedBaseURL("https://" + host + "/v1")
		require.NoError(t, err, host)
		require.Equal(t, "https://"+host+"/v1", validated)
	}
	_, err := ValidateTrustedBaseURL("https://api.x.ai.attacker.example/v1")
	require.Error(t, err)
	require.False(t, IsParseableBaseURL("://invalid"))
}
