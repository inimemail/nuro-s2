package ssebudget

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReaderDiscardsWholeOversizedEventAndRetainsLateUsage(t *testing.T) {
	late := "data: {\"usage\":{\"input_tokens\":123}}\n\n"
	for name, tc := range map[string]struct {
		limits Limits
		bad    string
	}{
		"line":       {Limits{LineBytes: 128}, "data: " + strings.Repeat("secret", 10000) + "\n\n"},
		"event":      {Limits{EventBytes: 128}, strings.Repeat("data: secret\n", 20) + "\n"},
		"lines":      {Limits{EventLines: 3}, "data: secret\ndata: secret\ndata: secret\n\n"},
		"no-newline": {Limits{LineBytes: 128}, strings.Repeat("secret", 10000)},
	} {
		t.Run(name, func(t *testing.T) {
			input := tc.bad
			if name != "no-newline" {
				input += late
			}
			r := NewReader(strings.NewReader(input), tc.limits)
			out, err := io.ReadAll(r)
			require.ErrorIs(t, err, ErrExceeded)
			require.True(t, r.Exceeded())
			require.NotContains(t, string(out), "secret")
			if name != "no-newline" {
				require.Equal(t, late, string(out))
			}
			_, err = r.Read(make([]byte, 1))
			require.ErrorIs(t, err, io.EOF)
		})
	}
}
func TestReaderPreservesFramingAtExactBoundary(t *testing.T) {
	for _, input := range []string{"data: {}\n\n", ": ping\r\ndata: {}\r\n\r\n", "data: {}"} {
		r := NewReader(strings.NewReader(input), Limits{EventBytes: len(input), LineBytes: len(input), EventLines: 4})
		out, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, input, string(out))
		require.False(t, r.Exceeded())
	}
}
func TestQueueCancellationReleaseAndExclusiveLargeEvent(t *testing.T) {
	q := NewQueue(10)
	done := make(chan struct{})
	require.True(t, q.Acquire(100, done))
	acquired := make(chan bool, 1)
	go func() { acquired <- q.Acquire(1, done) }()
	select {
	case <-acquired:
		t.Fatal("queue exceeded byte budget")
	case <-time.After(10 * time.Millisecond):
	}
	q.Release(100)
	select {
	case ok := <-acquired:
		require.True(t, ok)
	case <-time.After(time.Second):
		t.Fatal("release did not wake producer")
	}
	close(done)
	require.False(t, q.Acquire(10, done))
	q.Release(1)
	require.Zero(t, q.used)
	require.Nil(t, NewQueue(0))
}
func FuzzReaderBoundaries(f *testing.F) {
	f.Add("data: {}\r\n\r\n", uint16(8))
	f.Add(strings.Repeat("x", 9000), uint16(128))
	f.Fuzz(func(t *testing.T, input string, limit uint16) {
		if len(input) > 1<<20 {
			t.Skip()
		}
		r := NewReader(strings.NewReader(input), Limits{LineBytes: int(limit) + 1, EventBytes: int(limit) + 1, EventLines: 100})
		_, err := io.ReadAll(r)
		if r.Exceeded() {
			require.ErrorIs(t, err, ErrExceeded)
		} else {
			require.NoError(t, err)
		}
	})
}
