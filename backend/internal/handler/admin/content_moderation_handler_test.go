package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseOptionalProxyIDPreservesPatchSemantics(t *testing.T) {
	var omitted struct {
		ProxyID json.RawMessage `json:"proxy_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{}`), &omitted))
	id, err := parseOptionalProxyID(omitted.ProxyID)
	require.NoError(t, err)
	require.Nil(t, id)

	var cleared struct {
		ProxyID json.RawMessage `json:"proxy_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"proxy_id":null}`), &cleared))
	id, err = parseOptionalProxyID(cleared.ProxyID)
	require.NoError(t, err)
	require.NotNil(t, id)
	require.Zero(t, *id)

	var selected struct {
		ProxyID json.RawMessage `json:"proxy_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"proxy_id":42}`), &selected))
	id, err = parseOptionalProxyID(selected.ProxyID)
	require.NoError(t, err)
	require.NotNil(t, id)
	require.Equal(t, int64(42), *id)

	var invalid struct {
		ProxyID json.RawMessage `json:"proxy_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"proxy_id":"42"}`), &invalid))
	_, err = parseOptionalProxyID(invalid.ProxyID)
	require.Error(t, err)
}
