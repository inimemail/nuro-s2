package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesServiceTierPricingMetadataDoesNotChangeWire(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`"priority"`, "priority"}, {`"flex"`, "flex"}, {`null`, ""}, {`123`, ""}, {`{"name":"priority"}`, ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var response ResponsesResponse
			require.NoError(t, json.Unmarshal([]byte(`{"id":"resp_1","model":"custom-text","status":"completed","service_tier":`+tc.raw+`,"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`), &response))
			require.Equal(t, tc.want, response.UpstreamServiceTier)
			require.Equal(t, "resp_1", response.ID)
			require.Equal(t, 10, response.Usage.InputTokens)
			encoded, err := json.Marshal(response)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "service_tier")
			require.NoError(t, json.Unmarshal([]byte(`{"id":"resp_2"}`), &response))
			require.Empty(t, response.UpstreamServiceTier, "reused response objects must not inherit a tier")
		})
	}
}
