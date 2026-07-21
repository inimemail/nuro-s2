package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAbortWithOpenAIQuotaErrorV162UsesStandardEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	abortWithOpenAIQuotaError(c, http.StatusTooManyRequests, "quota exhausted")

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Equal(t, "quota exhausted", gjson.Get(recorder.Body.String(), "error.message").String())
	require.Equal(t, "insufficient_quota", gjson.Get(recorder.Body.String(), "error.type").String())
	require.Equal(t, "insufficient_quota", gjson.Get(recorder.Body.String(), "error.code").String())
	require.True(t, gjson.Get(recorder.Body.String(), "error.param").Exists())
	require.Equal(t, gjson.Null, gjson.Get(recorder.Body.String(), "error.param").Type)
	require.True(t, c.IsAborted())
}
