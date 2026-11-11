package antigravity

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestParallelToolResponsesPrecedeExtractedMedia(t *testing.T) {
	body := json.RawMessage(`[{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"first"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]},{"type":"tool_result","tool_use_id":"b","content":[{"type":"text","text":"second"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZGVm"}}]}]`)
	for _, nested := range []bool{false, true} {
		parts, _, err := buildParts(body, map[string]string{"a": "first", "b": "second"}, true, nested)
		require.NoError(t, err)
		if nested {
			require.Len(t, parts, 2)
			require.Len(t, parts[0].FunctionResponse.Parts, 1)
			require.Len(t, parts[1].FunctionResponse.Parts, 1)
		} else {
			require.Len(t, parts, 4)
			require.Equal(t, "a", parts[0].FunctionResponse.ID)
			require.Equal(t, "b", parts[1].FunctionResponse.ID)
			require.Equal(t, "YWJj", parts[2].InlineData.Data)
			require.Equal(t, "ZGVm", parts[3].InlineData.Data)
		}
	}
}
