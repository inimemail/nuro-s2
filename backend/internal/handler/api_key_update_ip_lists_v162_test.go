package handler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateAPIKeyRequestV162DistinguishesOmittedAndEmptyIPLists(t *testing.T) {
	var omitted UpdateAPIKeyRequest
	require.NoError(t, json.Unmarshal([]byte(`{"name":"unchanged"}`), &omitted))
	require.Nil(t, omitted.IPWhitelist)
	require.Nil(t, omitted.IPBlacklist)

	var cleared UpdateAPIKeyRequest
	require.NoError(t, json.Unmarshal([]byte(`{"ip_whitelist":[],"ip_blacklist":[]}`), &cleared))
	require.NotNil(t, cleared.IPWhitelist)
	require.NotNil(t, cleared.IPBlacklist)
	require.Empty(t, *cleared.IPWhitelist)
	require.Empty(t, *cleared.IPBlacklist)

	var updated UpdateAPIKeyRequest
	require.NoError(t, json.Unmarshal(
		[]byte(`{"ip_whitelist":["192.0.2.1"],"ip_blacklist":["198.51.100.0/24"]}`),
		&updated,
	))
	require.Equal(t, []string{"192.0.2.1"}, *updated.IPWhitelist)
	require.Equal(t, []string{"198.51.100.0/24"}, *updated.IPBlacklist)
}
