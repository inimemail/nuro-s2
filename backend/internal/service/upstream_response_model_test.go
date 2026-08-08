package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpstreamResponseModelObserverPrefersTerminalModel(t *testing.T) {
	o := &upstreamResponseModelObserver{}
	o.ObserveOpenAI([]byte(`{"type":"response.created","model":"gpt-5.6"}`), "response.created")
	o.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"model":"gpt-5.6"}}`), "response.completed")
	require.Equal(t, "gpt-5.6", o.Model())
	require.False(t, o.Conflict())
}

func TestUpstreamResponseModelObserverMarksConflictingFrames(t *testing.T) {
	o := &upstreamResponseModelObserver{}
	o.ObserveOpenAI([]byte(`{"type":"response.created","model":"gpt-5.6"}`), "response.created")
	o.ObserveOpenAI([]byte(`{"type":"response.done","response":{"model":"gpt-5.5"}}`), "response.done")
	require.Equal(t, "gpt-5.5", o.Model())
	require.True(t, o.Conflict())
}

func TestUpstreamResponseModelObserverResetsConflictForNextWSTurn(t *testing.T) {
	o := &upstreamResponseModelObserver{}
	o.ObserveOpenAI([]byte(`{"type":"response.created","response":{"model":"gpt-5.6"}}`), "response.created")
	o.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"model":"gpt-5.5"}}`), "response.completed")
	require.True(t, o.Conflict())

	o.ObserveOpenAI([]byte(`{"type":"response.created","response":{"model":"gpt-5.4"}}`), "response.created")
	o.ObserveOpenAI([]byte(`{"type":"response.completed","response":{"model":"gpt-5.4"}}`), "response.completed")
	require.Equal(t, "gpt-5.4", o.Model())
	require.False(t, o.Conflict())
}

func TestUpstreamModelMismatchIsTriStateAndCaseInsensitive(t *testing.T) {
	require.Nil(t, upstreamModelMismatch("gpt-5.6", ""))
	matched := upstreamModelMismatch("GPT-5.6", "gpt-5.6")
	require.NotNil(t, matched)
	require.False(t, *matched)
	mismatched := upstreamModelMismatch("gpt-5.6", "gpt-5.5")
	require.NotNil(t, mismatched)
	require.True(t, *mismatched)
}

func TestUpstreamModelMismatchWithConflict(t *testing.T) {
	conflicting := upstreamModelMismatchWithConflict("gpt-5.6", "gpt-5.6", true)
	require.NotNil(t, conflicting)
	require.True(t, *conflicting)

	matched := upstreamModelMismatchWithConflict("gpt-5.6", "gpt-5.6", false)
	require.NotNil(t, matched)
	require.False(t, *matched)
}

func TestNormalizeObservedUpstreamResponseModelBoundsRunes(t *testing.T) {
	value := normalizeObservedUpstreamResponseModel("  \u4e2d\u6587  ")
	require.Equal(t, "\u4e2d\u6587", value)
	long := normalizeObservedUpstreamResponseModel(string(make([]rune, upstreamResponseModelMaxLength+10)))
	require.Len(t, []rune(long), upstreamResponseModelMaxLength)
}
