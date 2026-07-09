//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasCompactionTriggerInInput_DetectsCompactSignal(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.5",
		"stream":true,
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction_trigger"}
		]
	}`)
	require.True(t, HasCompactionTriggerInInput(body))
}

func TestHasCompactionTriggerInInput_NoTrigger(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"hello"}]}`)
	require.False(t, HasCompactionTriggerInInput(body))
}

func TestHasCompactionTriggerInInput_NonArrayInput(t *testing.T) {
	require.False(t, HasCompactionTriggerInInput([]byte(`{"model":"gpt-5.5","input":"compaction_trigger"}`)))
	require.False(t, HasCompactionTriggerInInput([]byte(`{"model":"gpt-5.5"}`)))
	require.False(t, HasCompactionTriggerInInput(nil))
}

func TestHasCompactionTriggerInInput_CompactTriggerOnly(t *testing.T) {
	require.True(t, HasCompactionTriggerInInput([]byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"}]}`)))
}
