package service

import (
	"bytes"
	"io"
	"mime/multipart"
	"strings"
)

// NextPart can also return EOF for truncated MIME headers. A successful end
// must come from the closing delimiter, not exhaustion of the request body.
// Complete a possible final delimiter line using its original newline style,
// then report unexpected EOF if the parser reads past it. Uploaded bytes are
// never modified, and valid preambles, epilogues and LF bodies stay supported.
func newImageMultipartReader(body []byte, boundary string) *multipart.Reader {
	return multipart.NewReader(io.MultiReader(bytes.NewReader(body),
		strings.NewReader(imageMultipartLineEnding(body, boundary)), imageMultipartUnexpectedEOF{}), boundary)
}

type imageMultipartUnexpectedEOF struct{}

func (imageMultipartUnexpectedEOF) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func imageMultipartLineEnding(body []byte, boundary string) string {
	marker := []byte("--" + boundary)
	for start := 0; start < len(body); {
		at := bytes.Index(body[start:], marker)
		if at < 0 {
			break
		}
		at += start
		if at == 0 || body[at-1] == '\n' {
			tail := bytes.TrimLeft(body[at+len(marker):], " \t")
			if bytes.HasPrefix(tail, []byte("\r\n")) {
				return "\r\n"
			}
			if bytes.HasPrefix(tail, []byte("\n")) {
				return "\n"
			}
		}
		start = at + len(marker)
	}
	return "\r\n"
}
