package requestfields

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HasConflict detects ambiguous fields before routing, policy, or billing
// interprets the request. JSON detection uses the body itself.
func HasConflict(body []byte, contentType string) bool {
	document := bytes.TrimLeftFunc(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf}), func(r rune) bool { return r <= ' ' })
	isJSON := len(document) > 0 && (document[0] == '{' || document[0] == '[')
	if isJSON && hasJSONConflict(document) {
		return true
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "multipart/") {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return false
	}

	seen := make(map[string]struct{})
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextRawPart()
		if err != nil {
			return err != io.EOF && len(seen) > 0
		}
		if isJSON || multipartExtendedName(part.Header.Get("Content-Disposition")) {
			return true
		}
		name := strings.ToLower(part.FormName())
		switch name {
		case "model", "n", "size", "quality", "response_format", "stream", "service_tier", "background", "output_format", "output_compression", "moderation", "partial_images", "prompt":
			if _, exists := seen[name]; exists {
				return true
			}
			seen[name] = struct{}{}
		}
	}
}

func multipartExtendedName(disposition string) bool {
	// Inspect parameter names outside quoted values. MIME decoding itself
	// resolves extended names, losing the distinction from a plain name.
	quoted, escaped := false, false
	for i := 0; i < len(disposition); i++ {
		switch {
		case escaped:
			escaped = false
		case quoted && disposition[i] == '\\':
			escaped = true
		case disposition[i] == '"':
			quoted = !quoted
		case !quoted && disposition[i] == ';':
			rest := strings.TrimLeft(disposition[i+1:], " \t")
			if len(rest) >= len("name*") && strings.EqualFold(rest[:len("name*")], "name*") {
				return true
			}
		}
	}
	return false
}

type jsonFieldScanner struct {
	body     []byte
	pos      int
	conflict bool
}

func hasJSONConflict(document []byte) bool {
	scanner := jsonFieldScanner{body: document}
	scanner.value(0, false)
	return scanner.conflict
}

func (s *jsonFieldScanner) skipSpace() {
	for s.pos < len(s.body) && s.body[s.pos] <= ' ' {
		s.pos++
	}
}

// scanString consumes a string token without decoding string values.
func (s *jsonFieldScanner) scanString() (start int, end int, escaped bool, ok bool) {
	start = s.pos
	s.pos++
	rest := s.body[s.pos:]
	quote := bytes.IndexByte(rest, '"')
	if quote >= 0 && bytes.IndexByte(rest[:quote], '\\') < 0 {
		s.pos += quote + 1
		return start, s.pos, false, true
	}
	for s.pos < len(s.body) {
		switch s.body[s.pos] {
		case '"':
			s.pos++
			return start, s.pos, escaped, true
		case '\\':
			escaped = true
			s.pos++
		}
		s.pos++
	}
	return start, 0, escaped, false
}

func (s *jsonFieldScanner) value(depth int, freeform bool) bool {
	s.skipSpace()
	if s.pos >= len(s.body) {
		return false
	}
	switch s.body[s.pos] {
	case '{', '[':
		if depth >= 10000 {
			s.conflict = true
			return false
		}
		return s.container(depth, freeform)
	case '"':
		_, _, _, ok := s.scanString()
		return ok
	default:
		start := s.pos
		for s.pos < len(s.body) {
			c := s.body[s.pos]
			if c <= ' ' || c == ',' || c == '}' || c == ']' {
				break
			}
			s.pos++
		}
		return s.pos > start
	}
}

func (s *jsonFieldScanner) container(depth int, freeform bool) bool {
	object := s.body[s.pos] == '{'
	closeByte := byte(']')
	if object {
		closeByte = '}'
	}
	s.pos++
	s.skipSpace()
	if s.pos < len(s.body) && s.body[s.pos] == closeByte {
		s.pos++
		return true
	}

	var firstNames [8]string
	nameCount := 0
	var seen map[string]struct{}
	for s.pos < len(s.body) {
		childFreeform := freeform
		if object {
			if s.body[s.pos] != '"' {
				return false
			}
			start, end, escaped, ok := s.scanString()
			if !ok {
				return false
			}
			key := s.body[start+1 : end-1]
			var name string
			if escaped || !utf8.Valid(key) {
				if json.Unmarshal(s.body[start:end], &name) != nil {
					return false
				}
			} else {
				name = string(key)
			}
			if !freeform {
				name = foldName(name)
			}
			if seen != nil {
				if _, found := seen[name]; found {
					s.conflict = true
					return false
				}
				seen[name] = struct{}{}
			} else {
				for _, previous := range firstNames[:nameCount] {
					if previous == name {
						s.conflict = true
						return false
					}
				}
				if nameCount < len(firstNames) {
					firstNames[nameCount] = name
					nameCount++
				} else {
					seen = make(map[string]struct{}, len(firstNames)+1)
					for _, previous := range firstNames {
						seen[previous] = struct{}{}
					}
					seen[name] = struct{}{}
				}
			}
			if !freeform {
				switch name {
				case "properties", "patternproperties", "definitions", "$defs", "dependentschemas",
					"dependentrequired", "dependencies", "default", "const", "examples", "enum",
					"args", "arguments", "variables", "headers", "labels":
					childFreeform = true
				}
			}
			s.skipSpace()
			if s.pos >= len(s.body) || s.body[s.pos] != ':' {
				return false
			}
			s.pos++
		}
		if !s.value(depth+1, childFreeform) {
			return false
		}
		s.skipSpace()
		if s.pos >= len(s.body) {
			return false
		}
		c := s.body[s.pos]
		s.pos++
		if c == closeByte {
			return true
		}
		if c != ',' {
			return false
		}
		s.skipSpace()
	}
	return false
}

func foldName(input string) string {
	return strings.Map(func(r rune) rune {
		if r < unicode.MaxASCII {
			if r >= 'A' && r <= 'Z' {
				return r + 'a' - 'A'
			}
			return r
		}
		for next := unicode.SimpleFold(r); next > r; next = unicode.SimpleFold(r) {
			r = next
		}
		return unicode.ToLower(unicode.SimpleFold(r))
	}, input)
}
