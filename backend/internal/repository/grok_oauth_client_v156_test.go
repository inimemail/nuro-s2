//go:build unit

package repository

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestGrokOAuthStatusErrorPreservesOnlySafeIdentifier(t *testing.T) {
	err := newGrokOAuthStatusError(
		"GROK_OAUTH_TOKEN_REFRESH_FAILED",
		"token refresh failed",
		http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"token rejected by https://oauth.provider.example/token"}`,
	)

	require.ErrorContains(t, err, "invalid_grant")
	status := infraerrors.FromError(err)
	require.Equal(t, "token refresh failed: status 400 (invalid_grant)", status.Message)
	for _, forbidden := range []string{"error_description", "token rejected", "provider.example", "https://"} {
		require.NotContains(t, err.Error(), forbidden)
		require.NotContains(t, status.Message, forbidden)
	}
}

func TestGrokOAuthStatusErrorDropsUntrustedBody(t *testing.T) {
	body := `<!DOCTYPE html><title>502 at upstream.example</title><p>proxy http://10.0.0.8:8080 failed</p>`
	err := newGrokOAuthStatusError(
		"GROK_OAUTH_TOKEN_REFRESH_FAILED",
		"token refresh failed",
		http.StatusBadGateway,
		body,
	)

	status := infraerrors.FromError(err)
	require.Equal(t, "token refresh failed: status 502", status.Message)
	for _, forbidden := range []string{"DOCTYPE", "upstream.example", "10.0.0.8", "http://"} {
		require.NotContains(t, err.Error(), forbidden)
		require.NotContains(t, status.Message, forbidden)
	}
}

func TestGrokOAuthTransportErrorHasSafePublicMessageAndInternalCause(t *testing.T) {
	cause := errors.New("dial tcp oauth.provider.example:443: connection refused")
	err := infraerrors.New(http.StatusBadGateway, "GROK_OAUTH_REQUEST_FAILED", "OAuth request failed").WithCause(cause)

	status := infraerrors.FromError(err)
	require.Equal(t, "OAuth request failed", status.Message)
	require.NotContains(t, status.Message, "provider.example")
	require.True(t, errors.Is(err, cause))
	require.True(t, strings.Contains(err.Error(), "provider.example"), "internal error chain should retain the cause for logs")
}
