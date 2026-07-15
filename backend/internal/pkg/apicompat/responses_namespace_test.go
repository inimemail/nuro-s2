package apicompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlattenResponsesNamespaces_RewritesDeclarationHistoryAndChoice(t *testing.T) {
	req := map[string]any{
		"model": "gpt-5.5",
		"tools": []any{
			map[string]any{"type": "function", "name": "plain", "description": "keep"},
			map[string]any{
				"type": "namespace",
				"name": "collaboration",
				"tools": []any{
					map[string]any{"type": "function", "name": "spawn_agent", "description": "spawn", "parameters": map[string]any{"type": "object"}},
				},
			},
		},
		"tool_choice": map[string]any{"type": "function", "name": "spawn_agent", "namespace": "collaboration"},
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "spawn_agent", "namespace": "collaboration", "arguments": "{}"},
			map[string]any{"type": "message", "role": "user", "content": "hi", "name": "spawn_agent", "namespace": "collaboration"},
		},
	}

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, ResponsesNamespaceName{Namespace: "collaboration", Name: "spawn_agent"}, names["collaboration__spawn_agent"])

	tools := req["tools"].([]any)
	require.Len(t, tools, 2)
	require.Equal(t, "plain", tools[0].(map[string]any)["name"])
	require.Equal(t, "collaboration__spawn_agent", tools[1].(map[string]any)["name"])

	choice := req["tool_choice"].(map[string]any)
	require.Equal(t, "collaboration__spawn_agent", choice["name"])
	require.NotContains(t, choice, "namespace")

	input := req["input"].([]any)
	call := input[0].(map[string]any)
	require.Equal(t, "collaboration__spawn_agent", call["name"])
	require.NotContains(t, call, "namespace")
	message := input[1].(map[string]any)
	require.Equal(t, "spawn_agent", message["name"])
	require.Equal(t, "collaboration", message["namespace"])
}

func TestFlattenResponsesNamespaces_RejectsNameCollisions(t *testing.T) {
	t.Run("top level", func(t *testing.T) {
		req := map[string]any{"tools": []any{
			map[string]any{"type": "function", "name": "collaboration__spawn_agent"},
			map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
				map[string]any{"type": "function", "name": "spawn_agent"},
			}},
		}}

		_, _, err := FlattenResponsesNamespaces(req)
		require.ErrorContains(t, err, "conflicts with top-level tool")
	})

	t.Run("between namespaces", func(t *testing.T) {
		req := map[string]any{"tools": []any{
			map[string]any{"type": "namespace", "name": "a", "tools": []any{
				map[string]any{"type": "function", "name": "b__c"},
			}},
			map[string]any{"type": "namespace", "name": "a__b", "tools": []any{
				map[string]any{"type": "function", "name": "c"},
			}},
		}}

		_, _, err := FlattenResponsesNamespaces(req)
		require.ErrorContains(t, err, "conflict at")
	})
}

func TestFlattenResponsesNamespaces_HandlesNamespaceChoices(t *testing.T) {
	t.Run("flattened group falls back to auto", func(t *testing.T) {
		req := map[string]any{
			"tools": []any{map[string]any{
				"type": "namespace", "name": "collaboration", "tools": []any{
					map[string]any{"type": "function", "name": "spawn_agent"},
				},
			}},
			"tool_choice": map[string]any{"type": "namespace", "name": "collaboration"},
		}

		_, changed, err := FlattenResponsesNamespaces(req)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "auto", req["tool_choice"])
	})

	t.Run("image generation namespace is preserved", func(t *testing.T) {
		req := map[string]any{
			"tools": []any{
				map[string]any{"type": "namespace", "name": "image_gen", "tools": []any{
					map[string]any{"type": "function", "name": "imagegen"},
				}},
				map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
					map[string]any{"type": "function", "name": "spawn_agent"},
				}},
			},
			"tool_choice": map[string]any{"type": "namespace", "name": "image_gen"},
		}

		names, changed, err := FlattenResponsesNamespacesExcept(req, map[string]bool{"image_gen": true})
		require.NoError(t, err)
		require.True(t, changed)
		require.Contains(t, names, "collaboration__spawn_agent")
		tools := req["tools"].([]any)
		require.Equal(t, "namespace", tools[0].(map[string]any)["type"])
		require.Equal(t, "image_gen", tools[0].(map[string]any)["name"])
		require.Equal(t, map[string]any{"type": "namespace", "name": "image_gen"}, req["tool_choice"])
	})
}

func TestFlattenResponsesNamespaces_HandlesAdditionalToolsCarrier(t *testing.T) {
	req := map[string]any{
		"input": []any{
			map[string]any{
				"type": "additional_tools",
				"tools": []any{
					map[string]any{"type": "function", "name": "plain"},
					map[string]any{"type": "namespace", "name": "collaboration", "tools": []any{
						map[string]any{"type": "function", "name": "send_message", "parameters": map[string]any{"type": "object"}},
					}},
				},
			},
			map[string]any{"type": "function_call", "name": "send_message", "namespace": "collaboration", "arguments": "{}"},
		},
		"tool_choice": map[string]any{"type": "function", "name": "send_message", "namespace": "collaboration"},
	}

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, ResponsesNamespaceName{Namespace: "collaboration", Name: "send_message"}, names["collaboration__send_message"])

	input := req["input"].([]any)
	additional := input[0].(map[string]any)
	tools := additional["tools"].([]any)
	require.Len(t, tools, 2)
	require.Equal(t, "plain", tools[0].(map[string]any)["name"])
	require.Equal(t, "collaboration__send_message", tools[1].(map[string]any)["name"])
	require.Equal(t, "collaboration__send_message", input[1].(map[string]any)["name"])
	require.Equal(t, "collaboration__send_message", req["tool_choice"].(map[string]any)["name"])
}

func TestRestoreResponsesNamespaceCalls_RewritesFunctionCallLifecycle(t *testing.T) {
	names := map[string]ResponsesNamespaceName{
		"collaboration__spawn_agent": {Namespace: "collaboration", Name: "spawn_agent"},
	}
	for _, payload := range []string{
		`{"type":"response.output_item.added","item":{"type":"function_call","name":"collaboration__spawn_agent","arguments":"{}"}}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","name":"collaboration__spawn_agent","arguments":"{}"}}`,
		`{"type":"response.completed","response":{"output":[{"type":"function_call","name":"collaboration__spawn_agent","arguments":"{}"},{"type":"message","name":"collaboration__spawn_agent"}]}}`,
	} {
		got, changed, err := RestoreResponsesNamespaceCalls([]byte(payload), names)
		require.NoError(t, err)
		require.True(t, changed)
		require.Contains(t, string(got), `"name":"spawn_agent"`)
		require.Contains(t, string(got), `"namespace":"collaboration"`)
	}
}
