package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilterCodexInput_StripsFunctionCallItemID_WhenPreservingReferences(t *testing.T) {
	input := []any{
		map[string]any{
			"type":    "function_call",
			"id":      "item_A9v0SNfS3VaLrfX0j3y4xhyK",
			"call_id": "fc_abc123",
			"name":    "bash",
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": "fc_abc123",
			"output":  "done",
		},
	}

	filtered := filterCodexInputWithOptions(input, codexInputFilterOptions{
		PreserveReferences: true,
	})

	require.Len(t, filtered, 2)
	fc, ok := filtered[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function_call", fc["type"])
	require.NotContains(t, fc, "id")
	require.Equal(t, "fc_abc123", fc["call_id"])
	require.Equal(t, "bash", fc["name"])
}

func TestFilterCodexInput_KeepsFcID_WhenPreservingReferences(t *testing.T) {
	input := []any{
		map[string]any{
			"type":    "function_call",
			"id":      "fc_validID123",
			"call_id": "fc_validID123",
			"name":    "bash",
		},
	}

	filtered := filterCodexInputWithOptions(input, codexInputFilterOptions{
		PreserveReferences: true,
	})

	require.Len(t, filtered, 1)
	fc, ok := filtered[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fc_validID123", fc["id"])
}

func TestFilterCodexInput_StripsItemIDFromToolCallInputTypes(t *testing.T) {
	for _, typ := range []string{"function_call", "tool_call", "local_shell_call", "custom_tool_call", "mcp_tool_call"} {
		input := []any{
			map[string]any{
				"type":    typ,
				"id":      "item_xyz",
				"call_id": "fc_001",
				"name":    "tool",
			},
		}

		filtered := filterCodexInputWithOptions(input, codexInputFilterOptions{
			PreserveReferences: true,
		})

		require.Len(t, filtered, 1)
		item, ok := filtered[0].(map[string]any)
		require.True(t, ok)
		require.NotContains(t, item, "id", "item_* id should be stripped from %s", typ)
	}
}

func TestFilterCodexInput_StripsInvalidMessageIDAndPreservesOutputID(t *testing.T) {
	input := []any{
		map[string]any{
			"type":    "function_call_output",
			"id":      "o1",
			"call_id": "fc_abc",
			"output":  "done",
		},
		map[string]any{
			"type": "message",
			"id":   "item_msg_001",
			"role": "user",
		},
	}

	filtered := filterCodexInputWithOptions(input, codexInputFilterOptions{
		PreserveReferences: true,
	})

	require.Len(t, filtered, 2)
	out, ok := filtered[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "o1", out["id"])
	msg, ok := filtered[1].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, msg, "id")
}

func TestFilterCodexInput_PreservesValidMessageID(t *testing.T) {
	filtered := filterCodexInputWithOptions([]any{map[string]any{
		"type": "message",
		"id":   "msg_001",
		"role": "assistant",
	}}, codexInputFilterOptions{PreserveReferences: true})

	require.Len(t, filtered, 1)
	message, ok := filtered[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "msg_001", message["id"])
}
