package service

import (
	"encoding/json"
	"unicode/utf16"
)

// Turn metadata is also an HTTP header. Preserve JSON semantics using ASCII escapes.
func marshalCodexTurnMetadata(metadata map[string]any) ([]byte, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	for i, b := range raw {
		if b < 0x7f {
			continue
		}
		out := append(make([]byte, 0, len(raw)+16), raw[:i]...)
		escape := func(r rune) {
			const hex = "0123456789abcdef"
			out = append(out, '\\', 'u', hex[r>>12&15], hex[r>>8&15], hex[r>>4&15], hex[r&15])
		}
		for _, r := range string(raw[i:]) {
			if r < 0x7f {
				out = append(out, byte(r))
			} else if r <= 0xffff {
				escape(r)
			} else {
				high, low := utf16.EncodeRune(r)
				escape(high)
				escape(low)
			}
		}
		return out, nil
	}
	return raw, nil
}
