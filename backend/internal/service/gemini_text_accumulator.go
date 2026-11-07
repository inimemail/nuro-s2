package service

import "strings"

// Stream-local state. Builder.String snapshots remain immutable while append
// writes only new bytes. Reset starts a new allocation before cumulative reuse.
type geminiTextAccumulator struct {
	seen      string
	buffer    strings.Builder
	appending bool
}

func (a *geminiTextAccumulator) reset() { a.seen = ""; a.buffer.Reset(); a.appending = false }
func (a *geminiTextAccumulator) delta(incoming string) string {
	incoming = strings.TrimSuffix(incoming, "\x00")
	if incoming == "" {
		return ""
	}
	if strings.HasPrefix(incoming, a.seen) {
		delta := incoming[len(a.seen):]
		a.seen = incoming
		a.buffer.Reset()
		a.appending = false
		return delta
	}
	if strings.HasPrefix(a.seen, incoming) {
		return ""
	}
	if !a.appending {
		_, _ = a.buffer.WriteString(a.seen)
		a.appending = true
	}
	_, _ = a.buffer.WriteString(incoming)
	a.seen = a.buffer.String()
	return incoming
}
