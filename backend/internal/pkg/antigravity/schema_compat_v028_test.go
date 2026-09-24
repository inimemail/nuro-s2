package antigravity

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestV028SchemaTupleCompatibility(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"type":"array","prefixItems":[{"type":"string"},{"type":"string"}],"items":false}`, true},
		{`{"type":"array","items":[{"type":"string"},{"type":"string"}],"additionalItems":{"type":"string"}}`, true},
		{`{"type":"array","prefixItems":[{"type":"string"},{"type":"number"}],"items":false}`, false},
		{`{"type":"array","prefixItems":[{"type":"string"}]}`, false},
		{`{"type":"array","items":[]}`, false},
		{`{"type":"object","properties":{"const":{"type":"string","const":"yes"},"required":{"type":"number"}}}`, true},
		{`{"type":"object","$defs":{"x":{"$ref":"#/$defs/x"}},"properties":{"a":{"$ref":"#/$defs/x"}}}`, false},
		{`{"type":"object","$defs":{"x":{"type":"string"}},"properties":{"a":{"$ref":"#/$defs/x"}}}`, true},
		{`{"const":null}`, false}, {`{"const":{"x":1}}`, false},
		{`{"type":"object","examples":[{"$ref":"ordinary data"}],"properties":{"$ref":{"type":"string"}}}`, true},
	} {
		var schema map[string]any
		require.NoError(t, json.Unmarshal([]byte(tc.body), &schema))
		err := NormalizeCompatibleSchema(schema)
		if tc.valid {
			require.NoError(t, err)
			clean := CleanJSONSchema(schema)
			require.NotNil(t, clean)
			require.NotContains(t, clean, "prefixItems")
		} else {
			require.Error(t, err)
		}
	}
}
