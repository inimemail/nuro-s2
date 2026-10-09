package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type codexModelsFailoverAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r codexModelsFailoverAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r codexModelsFailoverAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	accounts := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

type codexModelsFailoverHTTPUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	firstBody  string
	firstCode  int
}

func (u *codexModelsFailoverHTTPUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	if accountID == 1 {
		status := u.firstCode
		if status == 0 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(u.firstBody)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-5.6"}]}`)),
	}, nil
}

func (u *codexModelsFailoverHTTPUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func newCodexModelsFailoverHandler(t *testing.T, upstream *codexModelsFailoverHTTPUpstream, accountCount int) (*OpenAIGatewayHandler, int64) {
	t.Helper()
	accounts := make([]service.Account, 0, accountCount)
	for i := 1; i <= accountCount; i++ {
		accounts = append(accounts, service.Account{
			ID:          int64(i),
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
			Priority:    i - 1,
			Concurrency: 1,
			Credentials: map[string]any{
				"api_key":  "sk-test",
				"base_url": "https://private-upstream.example/v1",
			},
		})
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		codexModelsFailoverAccountRepo{accounts: accounts},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil,
		upstream, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	return &OpenAIGatewayHandler{gatewayService: gatewayService, maxAccountSwitches: 3}, 42
}

func performCodexModelsHandlerRequest(handler *OpenAIGatewayHandler, groupID int64) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.144.0", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		GroupID: &groupID,
		Group:   &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
	})
	handler.CodexModels(c)
	return recorder
}

func TestV0215CompositeCodexManifestIsFilteredAndConditional(t *testing.T) {
	groupID := int64(81)
	repo := &gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
		groupID: {
			{ID: 1, Platform: service.PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"public": "gpt-5.6", "private": "gpt-5.6"}}},
			{ID: 2, Platform: service.PlatformTypeSafe, Credentials: map[string]any{"model_mapping": map[string]any{"jev-latest": "jev-latest"}}},
		},
	}}
	svc := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, &config.Config{RunMode: config.RunModeSimple}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	h := &OpenAIGatewayHandler{gatewayService: svc}
	group := &service.Group{ID: groupID, Platform: service.PlatformComposite, ModelsListConfig: service.GroupModelsListConfig{Enabled: true, Models: []string{"public"}}}
	request := func(etag string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.153.0", nil)
		c.Request.Header.Set("If-None-Match", etag)
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: group})
		h.CodexModels(c)
		c.Writer.WriteHeaderNow()
		return recorder
	}
	response := request("")
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"slug":"public"`)
	require.NotContains(t, response.Body.String(), "private")
	require.NotContains(t, response.Body.String(), "jev-latest")
	require.Contains(t, response.Body.String(), `"supported_reasoning_levels":[]`)
	require.Contains(t, response.Body.String(), `"context_window":null`)
	require.Contains(t, response.Body.String(), `"instructions_template":""`)
	require.Equal(t, http.StatusNotModified, request(response.Header().Get("ETag")).Code)
	group.ModelsListConfig.Models = []string{"missing"}
	empty := request(response.Header().Get("ETag"))
	require.Equal(t, http.StatusOK, empty.Code)
	require.JSONEq(t, `{"models":[]}`, empty.Body.String())
}

func TestCodexModelsFailsOverFromInvalidManifestEnvelope(t *testing.T) {
	upstream := &codexModelsFailoverHTTPUpstream{firstBody: `{"object":"list","data":[]}`}
	handler, groupID := newCodexModelsFailoverHandler(t, upstream, 2)
	recorder := performCodexModelsHandlerRequest(handler, groupID)

	if got := upstream.calls(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("account calls = %v, want [1 2]", got)
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"models":[{"slug":"gpt-5.6"}]}` {
		t.Fatalf("response status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestCodexModelsPermanentFourHundredDoesNotFailOverOrLeak(t *testing.T) {
	upstream := &codexModelsFailoverHTTPUpstream{
		firstCode: http.StatusBadRequest,
		firstBody: `{"error":{"message":"private-upstream.example Authorization sk-secret"}}`,
	}
	handler, groupID := newCodexModelsFailoverHandler(t, upstream, 2)
	recorder := performCodexModelsHandlerRequest(handler, groupID)

	if got := upstream.calls(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("account calls = %v, want [1]", got)
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	body := recorder.Body.String()
	for _, forbidden := range []string{"private-upstream.example", "Authorization", "sk-secret"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
}

func TestCodexModelsCanceledRequestDoesNotWriteResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)

	(&OpenAIGatewayHandler{}).CodexModels(c)
	if c.Writer.Written() {
		t.Fatalf("canceled request wrote status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestCodexModelsRequiresOpenAIKeyGroupOnBothRoutes(t *testing.T) {
	for _, path := range []string{"/v1/models?client_version=0.144.0", "/backend-api/codex/models"} {
		for _, tc := range []struct {
			name   string
			key    *service.APIKey
			status int
		}{
			{"missing key", nil, http.StatusUnauthorized},
			{"missing group", &service.APIKey{}, http.StatusUnauthorized},
			{"grok", &service.APIKey{Group: &service.Group{Platform: service.PlatformGrok}}, http.StatusNotFound},
			{"anthropic", &service.APIKey{Group: &service.Group{Platform: service.PlatformAnthropic}}, http.StatusNotFound},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				upstream := &codexModelsFailoverHTTPUpstream{}
				handler, _ := newCodexModelsFailoverHandler(t, upstream, 1)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodGet, path, nil)
				if tc.key != nil {
					c.Set(string(middleware2.ContextKeyAPIKey), tc.key)
				}
				handler.CodexModels(c)
				if w.Code != tc.status || len(upstream.calls()) != 0 {
					t.Fatalf("status=%d upstream=%v", w.Code, upstream.calls())
				}
			})
		}
	}
}
