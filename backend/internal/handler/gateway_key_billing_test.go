package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type keyBillingGroupRepository struct {
	service.GroupRepository
	group *service.Group
	err   error
	ids   []int64
}

func (r *keyBillingGroupRepository) GetByIDLite(ctx context.Context, id int64) (*service.Group, error) {
	r.ids = append(r.ids, id)
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing bounded group read deadline")
	}
	return r.group, r.err
}

func TestKeyBillingInfoIncludesOnlyBoundGroupOnRequest(t *testing.T) {
	group := &service.Group{
		ID: 17, Status: service.StatusActive, Name: "Example group", Description: "Public description", RateMultiplier: 1.25,
		I18n: service.GroupI18n{"zh": {Name: "示例分组", Description: "分组简介"}},
	}
	repo := &keyBillingGroupRepository{group: group}
	apiKeys := service.NewAPIKeyService(nil, nil, repo, nil, nil, nil, nil)
	h := &GatewayHandler{apiKeyService: apiKeys}
	get := func(query string, authenticated bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/sub2api/billing"+query, nil)
		c.Request.Header.Set("Accept-Language", "zh-CN")
		if authenticated {
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
		}
		h.KeyBillingInfo(c)
		return w
	}

	for _, query := range []string{"", "?include=other,grouped"} {
		response := get(query, true)
		require.Equal(t, http.StatusOK, response.Code)
		require.False(t, gjson.Get(response.Body.String(), "group_name").Exists())
		require.False(t, gjson.Get(response.Body.String(), "group_description").Exists())
		require.Equal(t, 1.25, gjson.Get(response.Body.String(), "effective_rate_multiplier").Float())
	}
	require.Empty(t, repo.ids)

	require.Equal(t, http.StatusUnauthorized, get("?include=group", false).Code)
	require.Empty(t, repo.ids)

	response := get("?include=other,%20group%20&include=group&group_id=99", true)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Equal(t, "示例分组", gjson.Get(response.Body.String(), "group_name").String())
	require.Equal(t, "分组简介", gjson.Get(response.Body.String(), "group_description").String())
	require.Equal(t, []int64{17}, repo.ids)

	repo.err = context.DeadlineExceeded
	response = get("?include=group", true)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, 1.25, gjson.Get(response.Body.String(), "effective_rate_multiplier").Float())
	require.False(t, gjson.Get(response.Body.String(), "group_description").Exists())
}
