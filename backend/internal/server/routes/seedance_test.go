package routes

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestV027SeedanceAliasesReachAuthenticatedHandler(t *testing.T) {
	router := newGatewayRoutesTestRouter()
	for _, prefix := range []string{"/api/v3", "/v3", "/v1", ""} {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			path := prefix + "/contents/generations/tasks"
			if method != http.MethodPost {
				path += "/task-1"
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{"model":"seedance","content":[{"type":"text","text":"hi"}]}`)))
			// The stub authenticates an OpenAI group but has no Seedance service.
			// 503 from the dedicated handler proves routing + authentication run.
			require.Equal(t, http.StatusServiceUnavailable, w.Code, "%s %s", method, path)
			require.Contains(t, w.Body.String(), "Seedance is unavailable")
		}
	}
}
