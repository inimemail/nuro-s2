package httputil

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type stalledBodyReader struct{}

func (stalledBodyReader) Read([]byte) (int, error) { return 0, nil }

func TestBodyChunksBoundariesAndLimit(t *testing.T) {
	for _, size := range []int{0, 511, 512, 513, 1 << 20, (3 << 20) + 7} {
		body := bytes.Repeat([]byte{'x'}, size)
		req := newRequestWithBody(t, body, "")
		req.ContentLength = 1 << 60 // Never allocate based on this untrusted hint.
		got, err := ReadRequestBodyWithPrealloc(req)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("size %d: %v", size, err)
		}
	}
	_, err := readRequestBodyChunks(stalledBodyReader{}, 512)
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("stalled reader: %v", err)
	}
	reader := http.MaxBytesReader(nil, io.NopCloser(strings.NewReader("too long")), 3)
	_, err = readRequestBodyChunks(reader, 512)
	var limit *http.MaxBytesError
	if !errors.As(err, &limit) {
		t.Fatalf("limit lost: %v", err)
	}
}
