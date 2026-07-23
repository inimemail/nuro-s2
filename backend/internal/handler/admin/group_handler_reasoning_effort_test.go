package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateGroupReasoningEffortMappingsTriState(t *testing.T) {
	var omitted UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{}`), &omitted))
	require.Nil(t, omitted.ReasoningEffortMappings)

	var clear UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{"reasoning_effort_mappings":[]}`), &clear))
	require.NotNil(t, clear.ReasoningEffortMappings)
	require.Empty(t, *clear.ReasoningEffortMappings)

	var replace UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{"reasoning_effort_mappings":[{"from":"max","to":"xhigh"}]}`), &replace))
	require.Len(t, *replace.ReasoningEffortMappings, 1)
}
