package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokClientToolsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	body, mapping, err := adaptGrokClientTools(c, []byte("{\"model\":\"grok\",\"tools\":[{\"type\":\"custom\",\"name\":\"draw\",\"description\":\"draw\"},{\"type\":\"tool_search\"}],\"input\":[{\"type\":\"custom_tool_call\",\"name\":\"draw\",\"input\":\"circle\"}]}"))
	require.NoError(t, err)
	require.True(t, mapping.custom["draw"])
	require.True(t, mapping.search)
	require.Equal(t, "function", gjson.GetBytes(body, "tools.0.type").String())
	require.Equal(t, "function_call", gjson.GetBytes(body, "input.0.type").String())
	require.Equal(t, grokToolSearchProxyName, gjson.GetBytes(body, "tools.1.name").String())

	restored, err := restoreGrokClientToolPayload(c, []byte("{\"output\":[{\"type\":\"function_call\",\"name\":\"draw\",\"arguments\":\"{\\\"input\\\":\\\"circle\\\"}\"},{\"type\":\"function_call\",\"name\":\"__sub2api_tool_search\",\"arguments\":\"{}\"}]}"))
	require.NoError(t, err)
	require.Equal(t, "custom_tool_call", gjson.GetBytes(restored, "output.0.type").String())
	require.Equal(t, "circle", gjson.GetBytes(restored, "output.0.input").String())
	require.Equal(t, "tool_search_call", gjson.GetBytes(restored, "output.1.type").String())
}

func TestGrokClientToolsRejectAmbiguousNames(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"type":"function","name":"draw"},{"type":"custom","name":"draw"}]}`,
		`{"tools":[{"type":"custom","name":"__sub2api_tool_search"}]}`,
	} {
		_, _, err := adaptGrokClientTools(nil, []byte(body))
		require.Error(t, err)
	}
}
