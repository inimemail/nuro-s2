package handler

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type v027RedeemRepo struct {
	service.RedeemCodeRepository
	calls int
}

func (r *v027RedeemRepo) ListByUser(_ context.Context, id int64, limit int) ([]service.RedeemCode, error) {
	r.calls++
	return []service.RedeemCode{{ID: id, Value: float64(limit)}}, nil
}
func (r *v027RedeemRepo) ListByUserPaginated(_ context.Context, id int64, p pagination.PaginationParams, _ string) ([]service.RedeemCode, *pagination.PaginationResult, error) {
	r.calls++
	return []service.RedeemCode{{ID: id}}, &pagination.PaginationResult{Total: 51, Page: p.Page, PageSize: p.PageSize, Pages: 3}, nil
}

func TestV027RedeemHistoryLegacyAndPagination(t *testing.T) {
	for _, query := range []string{"", "?page=2&page_size=25", "?page=0", "?page=1&page_size=101", "?page=9223372036854775807&page_size=100", "?page_size=abc"} {
		t.Run(query, func(t *testing.T) {
			repo := &v027RedeemRepo{}
			h := NewRedeemHandler(service.NewRedeemService(repo, nil, nil, nil, nil, nil, nil, nil))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/redeem/history"+query, nil)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 42})
			h.GetHistory(c)
			if query == "" {
				require.Equal(t, 200, w.Code)
				require.True(t, gjson.Get(w.Body.String(), "data").IsArray())
				require.Equal(t, int64(42), gjson.Get(w.Body.String(), "data.0.id").Int())
			} else if query == "?page=2&page_size=25" {
				require.Equal(t, 200, w.Code)
				require.Equal(t, int64(2), gjson.Get(w.Body.String(), "data.page").Int())
				require.Equal(t, int64(51), gjson.Get(w.Body.String(), "data.total").Int())
			} else {
				require.Equal(t, 400, w.Code)
				require.Zero(t, repo.calls)
			}
		})
	}
}

func TestV027GeminiMergePreservesUnknownMetadata(t *testing.T) {
	body := []byte(`{ "models":[{"name":"models/gemini-a","future":{"n":9007199254740993}}], "nextPageToken":"cursor" }`)
	out, ok := appendUpstreamGeminiModels(body, nil, nil)
	require.True(t, ok)
	require.Equal(t, body, out)
}

func TestV027SeedanceRespectsChannelBillingModelSource(t *testing.T) {
	for source, want := range map[string]string{"": "requested", service.BillingModelSourceRequested: "requested", service.BillingModelSourceChannelMapped: "routed", service.BillingModelSourceUpstream: "upstream"} {
		require.Equal(t, want, seedanceBillingModel(service.ChannelMappingResult{BillingModelSource: source}, "requested", "routed", "upstream"))
	}
}

type v027SeedanceLookupRepo struct {
	service.SeedanceTaskRepository
	task *service.SeedanceTask
}

func (r *v027SeedanceLookupRepo) Owned(context.Context, string, int64, int64, *int64) (*service.SeedanceTask, error) {
	return r.task, nil
}

func TestV027SeedanceLookupPreservesNullErrorAndSanitizesRealError(t *testing.T) {
	for _, body := range []string{
		`{"id":"task1","status":"succeeded","error":null,"content":{"video_url":"https://cdn.example/result.mp4"}}`,
		`{"id":"task1","status":"succeeded","content":{"video_url":"https://cdn.example/result.mp4"}}`,
		`{"id":"task1","status":"failed","error":{"code":"upstream_secret","message":"private upstream details"}}`,
	} {
		repo := &v027SeedanceLookupRepo{task: &service.SeedanceTask{ID: "local", ProviderID: "task1", State: "succeeded", Response: []byte(body)}}
		h := &OpenAIGatewayHandler{seedance: service.NewSeedanceService(repo, nil, nil)}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/api/v3/contents/generations/tasks/task1", nil)
		c.Params = gin.Params{{Key: "task_id", Value: "task1"}}
		h.seedanceLookup(c, &service.APIKey{ID: 2, UserID: 1})
		require.Equal(t, 200, w.Code)
		if gjson.Get(body, "status").String() == "succeeded" {
			require.JSONEq(t, body, w.Body.String())
		} else {
			require.Equal(t, "generation_failed", gjson.Get(w.Body.String(), "error.code").String())
			require.NotContains(t, w.Body.String(), "private upstream details")
		}
	}
}
