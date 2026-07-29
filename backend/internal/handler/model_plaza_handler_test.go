//go:build unit

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFilterModelPlazaGroupsProtectsExclusiveVisibility(t *testing.T) {
	groups := []service.ModelPlazaGroup{
		{ID: 1, Name: "public"},
		{ID: 2, Name: "exclusive-allowed", IsExclusive: true},
		{ID: 3, Name: "exclusive-denied", IsExclusive: true},
	}

	anonymous := filterModelPlazaGroups(groups, nil, false)
	require.Equal(t, []int64{1}, modelPlazaGroupIDs(anonymous))

	authenticated := filterModelPlazaGroups(groups, map[int64]struct{}{2: {}}, true)
	require.Equal(t, []int64{1, 2}, modelPlazaGroupIDs(authenticated))

	withoutGrants := filterModelPlazaGroups(groups, map[int64]struct{}{}, true)
	require.Equal(t, []int64{1}, modelPlazaGroupIDs(withoutGrants))
}

func TestModelPlazaGroupDTOIncludesUserRateAndWhitelistedPricing(t *testing.T) {
	input := 3e-6
	cacheRead := 3e-7
	group := service.ModelPlazaGroup{
		ID: 2, Name: "vip", Platform: service.PlatformAnthropic, IsExclusive: true,
		Models: []service.ModelPlazaModel{{
			Name: "claude-sonnet", Platform: service.PlatformAnthropic,
			Pricing:         &service.ChannelModelPricing{BillingMode: service.BillingModeToken, InputPrice: &input},
			OfficialPricing: &service.ModelPlazaOfficialPricing{InputPrice: &input, CacheReadPrice: &cacheRead},
		}},
	}

	dto := modelPlazaGroupDTO(&group, map[int64]float64{2: 0.5})
	raw, err := json.Marshal(dto)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.InDelta(t, 0.5, decoded["user_rate_multiplier"].(float64), 1e-12)
	require.NotContains(t, decoded, "account_id")
	require.NotContains(t, decoded, "upstream_url")

	models := decoded["models"].([]any)
	model := models[0].(map[string]any)
	require.Contains(t, model, "pricing")
	require.Contains(t, model, "official_pricing")
	require.NotContains(t, model, "channel_id")

	withoutRate, err := json.Marshal(modelPlazaGroupDTO(&group, nil))
	require.NoError(t, err)
	var decodedWithoutRate map[string]any
	require.NoError(t, json.Unmarshal(withoutRate, &decodedWithoutRate))
	require.NotContains(t, decodedWithoutRate, "user_rate_multiplier")
}

func TestModelPlazaHandlerFailsClosedWhenUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/model-plaza", nil)

	(&ModelPlazaHandler{}).Get(ctx)

	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func modelPlazaGroupIDs(groups []service.ModelPlazaGroup) []int64 {
	ids := make([]int64, 0, len(groups))
	for i := range groups {
		ids = append(ids, groups[i].ID)
	}
	return ids
}
