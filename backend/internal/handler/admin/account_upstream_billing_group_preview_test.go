package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPreviewUpstreamGroupAcceptsOnlyASavedAccountID(t *testing.T) {
	h := &AccountHandler{upstreamBillingProbe: &service.UpstreamBillingProbeService{}}
	request := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/upstream-billing-probe/group-preview", strings.NewReader(body))
		h.PreviewUpstreamGroup(c)
		return rec
	}
	for _, body := range []string{
		`{"account_id":0}`,
		`{"account_id":7,"api_key":"secret"}`,
		`{"account_id":7,"base_url":"https://private.example"}`,
		`{"account_id":7,"proxy_id":7}`,
		`{"account_id":7}{}`,
		`{"account_id":7}` + strings.Repeat(" ", 1024),
	} {
		rec := request(body)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		require.NotContains(t, rec.Body.String(), "secret")
		require.NotContains(t, rec.Body.String(), "private.example")
	}
	require.Equal(t, http.StatusServiceUnavailable, request(`{"account_id":7}`).Code)
}

func TestPreviewUpstreamGroupDeniesScopedAdminKey(t *testing.T) {
	const path = "/api/v1/admin/accounts/upstream-billing-probe/group-preview"
	scoped := &ScopedAdminHandler{}
	h := &AccountHandler{}
	router := gin.New()
	router.POST(path, func(c *gin.Context) {
		c.Set(adminaccess.PrincipalContextKey, &service.ScopedAdminPrincipal{})
	}, scoped.Intercept, h.PreviewUpstreamGroup)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"account_id":7}`)))
	require.Equal(t, http.StatusForbidden, rec.Code)
}
