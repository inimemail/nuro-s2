package httputil

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIdleDeadlinesZeroPreservesHandlerIdentity(t *testing.T) {
	h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	require.Equal(t, reflect.ValueOf(h).Pointer(), reflect.ValueOf(WithIdleDeadlines(h, 0, 0)).Pointer())
}
func TestIdleWriteDeadlineBoundsSlowH1AndH2Readers(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "h1", true: "h2"}[h2], func(t *testing.T) {
			finished := make(chan error, 1)
			h := WithIdleDeadlines(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				block := make([]byte, 64<<10)
				for range 1024 {
					if _, err := w.Write(block); err != nil {
						finished <- err
						return
					}
					if err := http.NewResponseController(w).Flush(); err != nil {
						finished <- err
						return
					}
				}
				finished <- nil
			}), 0, 40*time.Millisecond)
			server := httptest.NewUnstartedServer(h)
			server.EnableHTTP2 = h2
			if h2 {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			defer server.CloseClientConnections()
			resp, err := server.Client().Get(server.URL)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			select {
			case err := <-finished:
				require.Error(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("blocked downstream write did not time out")
			}
		})
	}
}
func TestIdleDeadlinesDoNotRemainOnHijackedConnection(t *testing.T) {
	finished := make(chan error, 1)
	h := WithIdleDeadlines(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			finished <- err
			return
		}
		defer func() { _ = conn.Close() }()
		// Deliberately outlive both idle settings after handing over ownership.
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		_, err = rw.WriteString("pong\n")
		if err == nil {
			err = rw.Flush()
		}
		finished <- err
	}), 20*time.Millisecond, 20*time.Millisecond)
	server := httptest.NewServer(h)
	defer server.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: fixture\r\n\r\n")
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	line, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "pong\n", line)
	require.NoError(t, <-finished)
}
func TestIdleDeadlineBoundsImplicitH1UnreadBodyDrain(t *testing.T) {
	finished := make(chan struct{})
	h := WithIdleDeadlines(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "rejected")
		_ = http.NewResponseController(w).Flush()
	}), 40*time.Millisecond, 100*time.Millisecond)
	server := httptest.NewServer(h)
	defer server.Close()
	defer server.CloseClientConnections()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "http://"), time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "POST / HTTP/1.1\r\nHost: fixture\r\nContent-Length: 100\r\n\r\nx")
	require.NoError(t, err)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("implicit request body drain was unbounded")
	}
}
