package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGrokTestErrorCategoryIsStableAndRedacted(t *testing.T) {
	tests := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, `{"error":"Bearer secret-token"}`, "token invalid"},
		{http.StatusForbidden, `{"error":"denied"}`, "permission denied"},
		{http.StatusNotFound, `{"error":"model not found"}`, "model not found"},
		{http.StatusNotFound, `{"error":"route missing"}`, "endpoint unsupported"},
		{http.StatusTooManyRequests, `{"error":"slow down"}`, "rate limited"},
		{http.StatusBadRequest, `{"error":"unknown parameter"}`, "unsupported request field"},
		{http.StatusServiceUnavailable, `{"error":"temporary"}`, "service unavailable"},
	}
	for _, tc := range tests {
		require.Equal(t, tc.want, grokTestErrorCategory(tc.status, []byte(tc.body)))
		require.NotContains(t, grokTestErrorCategory(tc.status, []byte(tc.body)), "secret-token")
	}
}
