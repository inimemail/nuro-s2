package httputil

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBodyBudgetReleaseAndAsyncOwnership(t *testing.T) {
	budget := NewBodyBudget(1024)
	scope := budget.Scope()
	require.NoError(t, scope.Reserve(512))
	release := scope.Detach()
	scope.Release()
	scope.Release()
	require.EqualValues(t, 512, budget.Used())
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(release)
	}
	wg.Wait()
	require.Zero(t, budget.Used())
	require.ErrorIs(t, scope.Reserve(1), ErrBodyBudgetExceeded)
	next := budget.Scope()
	require.NoError(t, next.Reserve(1024))
	require.ErrorIs(t, next.Reserve(1), ErrBodyBudgetExceeded)
	next.Release()
	require.Zero(t, budget.Used())
	require.Nil(t, NewBodyBudget(0).Scope())
}
func TestBodyBudgetExhaustionBeforeAllocationAndRecovery(t *testing.T) {
	budget := NewBodyBudget(2048)
	scope := budget.Scope()
	req := httptest.NewRequest("POST", "/", bytes.NewReader(bytes.Repeat([]byte("a"), 4096))).WithContext(WithBodyReservation(context.Background(), scope))
	_, err := ReadRequestBodyWithPrealloc(req)
	require.ErrorIs(t, err, ErrBodyBudgetExceeded)
	require.LessOrEqual(t, budget.Used(), int64(2048))
	scope.Release()
	require.Zero(t, budget.Used())
	scope = budget.Scope()
	req = httptest.NewRequest("POST", "/", bytes.NewReader([]byte("ok"))).WithContext(WithBodyReservation(context.Background(), scope))
	body, err := ReadRequestBodyWithPrealloc(req)
	require.NoError(t, err)
	require.Equal(t, "ok", string(body))
	scope.Release()
	require.Zero(t, budget.Used())
}
func TestDecompressedBodyRejectsOversizeInsteadOfTruncating(t *testing.T) {
	// The source produces bytes without holding a 64 MiB input buffer.
	_, err := readDecompressedBody(io.LimitReader(zeroReader{}, maxDecompressedBodySize+1))
	var tooLarge *http.MaxBytesError
	require.ErrorAs(t, err, &tooLarge)
	body, err := readDecompressedBody(bytes.NewReader([]byte("{}")))
	require.NoError(t, err)
	require.Equal(t, "{}", string(body))
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err = gz.Write(bytes.Repeat([]byte("x"), 256<<10))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	scope := NewBodyBudget(128 << 10).Scope()
	defer scope.Release()
	_, err = decompressRequestBody("gzip", compressed.Bytes(), scope)
	require.ErrorIs(t, err, ErrBodyBudgetExceeded)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestReadIdleDeadlineClassifiesStalledH1AndH2Uploads(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "h1", true: "h2"}[h2], func(t *testing.T) {
			result := make(chan error, 1)
			handler := WithIdleDeadlines(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.ReadAll(r.Body)
				result <- err
				if err != nil {
					w.WriteHeader(408)
				}
			}), 40*time.Millisecond, 0)
			server := httptest.NewUnstartedServer(handler)
			server.EnableHTTP2 = h2
			server.StartTLS()
			defer server.Close()
			reader, writer := io.Pipe()
			defer func() { _ = writer.Close() }()
			req, err := http.NewRequest("POST", server.URL, reader)
			require.NoError(t, err)
			req.ContentLength = 100
			done := make(chan struct{})
			go func() {
				resp, _ := server.Client().Do(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
				close(done)
			}()
			_, err = writer.Write([]byte("x"))
			require.NoError(t, err)
			select {
			case err := <-result:
				var timeout net.Error
				require.True(t, errors.As(err, &timeout) && timeout.Timeout())
			case <-time.After(2 * time.Second):
				t.Fatal("read idle deadline did not fire")
			}
			_ = writer.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not finish")
			}
		})
	}
}
