package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestV027SeedanceCORSAllowsRecoveryOnlyForConfiguredOrigins(t *testing.T) {
	r := gin.New()
	r.Use(CORS(config.CORSConfig{AllowedOrigins: []string{"https://console.example"}}))
	for _, origin := range []string{"https://console.example", "https://untrusted.example"} {
		req := httptest.NewRequest(http.MethodOptions, "/api/v3/contents/generations/tasks", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type, Idempotency-Key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if origin == "https://console.example" {
			require.Equal(t, http.StatusNoContent, w.Code)
			require.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key")
			require.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), "X-Seedance-Operation-ID")
		} else {
			require.Equal(t, http.StatusForbidden, w.Code)
			require.Empty(t, w.Header().Get("Access-Control-Expose-Headers"))
		}
	}
}
