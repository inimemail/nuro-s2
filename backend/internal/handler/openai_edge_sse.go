package handler

import (
	"bytes"

	"github.com/tidwall/gjson"
)

// Observe complete SSE frames without buffering or rewriting the forwarded
// stream. Oversized frames are ignored by the observer, never by the relay.
type openAIEdgeTerminalScanner struct {
	responses   bool
	seen        bool
	line, data  []byte
	event       string
	oversized   bool
	cr          bool
	lineHasData bool
}

func (s *openAIEdgeTerminalScanner) feed(chunk []byte) {
	for _, b := range chunk {
		if s.seen {
			return
		}
		if b == '\n' && s.cr {
			s.cr = false
			continue
		}
		s.cr = b == '\r'
		if b == '\n' || b == '\r' {
			s.endLine()
			continue
		}
		s.lineHasData = true
		if len(s.line)+len(s.data) >= 16<<20 {
			s.oversized = true
		}
		if !s.oversized {
			s.line = append(s.line, b)
		}
	}
}

func (s *openAIEdgeTerminalScanner) endLine() {
	if len(s.line) == 0 && !s.oversized {
		typ := s.event
		data := bytes.TrimSpace(s.data)
		validObject := gjson.ValidBytes(data) && gjson.ParseBytes(data).IsObject()
		if validObject {
			if value := gjson.GetBytes(data, "type"); value.Exists() {
				typ = value.String()
			}
		}
		if s.responses {
			if validObject {
				switch typ {
				case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "response.done":
					s.seen = true
				}
			}
		} else {
			s.seen = bytes.Equal(data, []byte("[DONE]"))
		}
		s.data = s.data[:0]
		s.event = ""
	} else if !s.oversized {
		field, value, _ := bytes.Cut(s.line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			s.event = string(value)
		case "data":
			s.data = append(s.data, value...)
			s.data = append(s.data, '\n')
		}
	}
	// Track blank lines even after the bounded observer discards a long line.
	if s.oversized && !s.lineHasData {
		s.oversized = false
		s.data = nil
		s.event = ""
	}
	s.line = s.line[:0]
	s.lineHasData = false
}
