package service

import (
	"github.com/stretchr/testify/require"
	"strconv"
	"strings"
	"testing"
)

func TestGeminiAccumulatorMatchesCompatibilityTrace(t *testing.T) {
	for _, trace := range [][]string{
		{"", "a", "ab", "a", "ab", "c", "d", "abcde", "z", "z\x00", "🙂", "\x00"},
		{"text", " delta", " more", "text delta more", "text delta more!"},
		{"a\x00\x00", "中文", "中", "終"},
	} {
		var a geminiTextAccumulator
		seen := ""
		for _, chunk := range trace {
			delta, next := computeGeminiTextDelta(seen, chunk)
			require.Equal(t, delta, a.delta(chunk))
			require.Equal(t, next, a.seen)
			seen = next
		}
	}
}
func FuzzGeminiTextAccumulator(f *testing.F) {
	f.Add("hello", " world", "hello world!")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		var acc geminiTextAccumulator
		seen := ""
		for _, chunk := range []string{a, b, c} {
			delta, next := computeGeminiTextDelta(seen, chunk)
			if acc.delta(chunk) != delta || acc.seen != next {
				t.Fatal("trace mismatch")
			}
			seen = next
		}
	})
}

var geminiBenchmarkSink string

func BenchmarkGeminiTextAccumulation(b *testing.B) {
	for _, mode := range []string{"delta", "cumulative", "mixed"} {
		chunks := make([]string, 1000)
		full := ""
		for i := range chunks {
			piece := strconv.Itoa(i) + "-" + strings.Repeat("x", 64)
			full += piece
			chunks[i] = piece
			if mode == "cumulative" || (mode == "mixed" && i%16 == 0) {
				chunks[i] = full
			}
		}
		b.Run(mode+"/previous", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				seen := ""
				for _, c := range chunks {
					_, seen = computeGeminiTextDelta(seen, c)
				}
				geminiBenchmarkSink = seen
			}
		})
		b.Run(mode+"/append", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var a geminiTextAccumulator
				for _, c := range chunks {
					a.delta(c)
				}
				geminiBenchmarkSink = a.seen
			}
		})
	}
}
