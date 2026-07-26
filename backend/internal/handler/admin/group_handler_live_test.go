package admin

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLiveCapabilityResponseDoesNotExposeProviderDiagnostics(t *testing.T) {
	result := liveCapabilityResponse(errors.New("/Applications/ChatGPT.app helper failed: secret diagnostics"))

	require.Equal(t, false, result["supported"])
	require.Equal(t, "unavailable", result["reason"])
	require.NotContains(t, result["reason"], "ChatGPT")
	require.NotContains(t, result["reason"], "/Applications")
}

func TestLiveCapabilityResponseReportsSupportedWithoutReason(t *testing.T) {
	result := liveCapabilityResponse(nil)

	require.Equal(t, true, result["supported"])
	_, hasReason := result["reason"]
	require.False(t, hasReason)
}
