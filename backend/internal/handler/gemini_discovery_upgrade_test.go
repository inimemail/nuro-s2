package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type v027GeminiDiscoveryRepo struct {
	service.AccountRepository
	mixed bool
}

func (r *v027GeminiDiscoveryRepo) ListSchedulableByGroupIDAndPlatforms(_ context.Context, _ int64, platforms []string) ([]service.Account, error) {
	if platforms[0] == service.PlatformAntigravity {
		return []service.Account{{ID: 2, Platform: service.PlatformAntigravity, Extra: map[string]any{"mixed_scheduling": r.mixed}, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-future-custom": "gemini-real", "gemini-variant-high": "high-target", "gemini-disabled": "", "gemini-disabled-high": "high-target", "gemini-unsafe/name": "gemini-real"}}}}, nil
	}
	return []service.Account{{ID: 1, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-only"}}}, nil
}
func (r *v027GeminiDiscoveryRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return &service.Account{ID: 1, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test-only"}}, nil
}

type v027GeminiDiscoveryUpstream struct {
	service.HTTPUpstream
	status int
}

func (u *v027GeminiDiscoveryUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	body := `{"error":{"message":"not found"}}`
	if u.status == http.StatusOK {
		body = `{"name":"models/gemini-future-custom","upstream_metadata":"preserved"}`
	}
	return &http.Response{StatusCode: u.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestV027GeminiAdvertisedMixedModelSurvivesNative404(t *testing.T) {
	for _, scenario := range []struct {
		mixed        bool
		status, want int
	}{{true, 404, 200}, {false, 404, 404}, {true, 503, 503}, {true, 200, 200}} {
		repo := &v027GeminiDiscoveryRepo{mixed: scenario.mixed}
		svc := service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, &v027GeminiDiscoveryUpstream{status: scenario.status}, nil, nil, nil, nil, &config.Config{})
		groupID := int64(3)
		ids, err := svc.AntigravityGeminiModelIDs(context.Background(), &groupID, true)
		require.NoError(t, err)
		require.NotContains(t, ids, "gemini-disabled")
		require.NotContains(t, ids, "gemini-unsafe/name")
		if scenario.mixed {
			require.Contains(t, ids, "gemini-variant")
		}
		h := &GatewayHandler{geminiCompatService: svc}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models/gemini-future-custom", nil)
		c.Params = gin.Params{{Key: "model", Value: "gemini-future-custom"}}
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformGemini}})
		h.GeminiV1BetaGetModel(c)
		require.Equal(t, scenario.want, w.Code, w.Body.String())
		if scenario.status == 200 {
			require.Equal(t, "preserved", gjson.Get(w.Body.String(), "upstream_metadata").String())
		}
	}
}
