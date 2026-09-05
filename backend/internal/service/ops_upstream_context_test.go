package service

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSafeUpstreamURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"strips query", "https://api.anthropic.com/v1/messages?beta=true", "https://api.anthropic.com/v1/messages"},
		{"strips fragment", "https://api.openai.com/v1/responses#frag", "https://api.openai.com/v1/responses"},
		{"strips both", "https://host/path?token=secret#x", "https://host/path"},
		{"no query or fragment", "https://host/path", "https://host/path"},
		{"empty string", "", ""},
		{"whitespace only", "  ", ""},
		{"query before fragment", "https://h/p?a=1#f", "https://h/p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, safeUpstreamURL(tt.input))
		})
	}
}

func TestAppendOpsUpstreamErrorDropsUpstreamURL(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		UpstreamStatusCode: 502,
		UpstreamURL:        "https://private.example/internal/endpoint",
		Message:            "request failed",
	})

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.Empty(t, events[0].UpstreamURL)
}

func TestAppendOpsUpstreamErrorSanitizesDetailBeforeContextStorage(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		UpstreamStatusCode: 502,
		Detail:             `{"error":{"message":"request to https://private.example failed","authorization":"Bearer secret-token"}}`,
	})

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.NotContains(t, events[0].Detail, "private.example")
	require.NotContains(t, events[0].Detail, "secret-token")
}

func TestOpsUpstreamProxyAttributionSnapshotsRoute(t *testing.T) {
	proxyID := int64(42)
	account := &Account{
		ID:      7,
		ProxyID: &proxyID,
		Proxy:   &Proxy{ID: proxyID, Name: "edge-eu"},
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	firstID, firstName := opsUpstreamProxyAttribution(account)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{ProxyID: firstID, ProxyName: firstName, Message: "first"})
	account.Proxy.Name = "changed-current-value"
	secondID, secondName := opsUpstreamProxyAttribution(account)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{ProxyID: secondID, ProxyName: secondName, Message: "second"})

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 2)
	require.NotNil(t, events[0].ProxyID)
	require.Equal(t, proxyID, *events[0].ProxyID)
	require.Equal(t, "edge-eu", events[0].ProxyName)
	require.Equal(t, "changed-current-value", events[1].ProxyName)

	_, gotName := opsUpstreamProxyAttribution(&Account{})
	require.Equal(t, opsProxyNameDirect, gotName)
	_, gotName = opsUpstreamProxyAttribution(&Account{Proxy: &Proxy{ID: proxyID}})
	require.Equal(t, opsProxyNameUnknown, gotName)
	_, gotName = opsUpstreamProxyAttribution(&Account{ProxyID: &proxyID})
	require.Equal(t, opsProxyNameUnknown, gotName)
	_, gotName = opsUpstreamProxyAttribution(&Account{ProxyID: &proxyID, Proxy: &Proxy{ID: 99, Name: "stale"}})
	require.Equal(t, opsProxyNameUnknown, gotName)
	_, gotName = opsUpstreamProxyAttribution(nil)
	require.Equal(t, opsProxyNameUnknown, gotName)
	_, gotName = opsUpstreamWSProxyAttribution(&Account{})
	require.Equal(t, opsProxyNameUnknown, gotName)
}

func TestNormalizeOpsUpstreamErrorsJSONPreservesLegacyFields(t *testing.T) {
	raw := `[{"message":"first","account_id":9},{"proxy_id":12,"message":"second"},{"proxy_name":"direct/no_proxy","message":"third"}]`
	normalized, err := normalizeOpsUpstreamErrorsJSON(raw)
	require.NoError(t, err)
	require.Contains(t, normalized, `"message":"first"`)
	require.Contains(t, normalized, `"message":"second"`)
	require.Contains(t, normalized, `"message":"third"`)
	// Existing key order and values are retained; only attribution keys are
	// added or corrected for compatibility with older stored events.
	require.Contains(t, normalized, `{"message":"first","account_id":9,"proxy_id":null,"proxy_name":"unknown"}`)
	require.Contains(t, normalized, `{"proxy_id":12,"message":"second","proxy_name":"proxy"}`)
	require.Contains(t, normalized, `{"proxy_name":"direct/no_proxy","message":"third","proxy_id":null}`)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal([]byte(normalized), &decoded))
	require.Len(t, decoded, 3)
	require.Equal(t, "unknown", decoded[0]["proxy_name"])
	require.Nil(t, decoded[0]["proxy_id"])
}
