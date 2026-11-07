package httputil

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// WithIdleDeadlines sets per-read/write deadlines; it never limits a stream's
// lifetime or cancels its upstream context. Both zero values preserve identity.
func WithIdleDeadlines(next http.Handler, readIdle, writeIdle time.Duration) http.Handler {
	if readIdle <= 0 && writeIdle <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		controller := http.NewResponseController(w)
		if readIdle > 0 && r.Body != nil {
			r.Body = &idleReadBody{ReadCloser: r.Body, controller: controller, idle: readIdle, req: r, w: w}
		}
		writer := &idleResponseWriter{ResponseWriter: w, controller: controller, idle: writeIdle, readIdle: readIdle, req: r}
		defer func() {
			if !writer.hijacked {
				// net/http may drain/flush after ServeHTTP returns. Keep those
				// operations bounded too; net/http clears deadlines before reuse.
				if r.ProtoMajor == 1 && readIdle > 0 {
					_ = controller.SetReadDeadline(time.Now().Add(readIdle))
				}
				if writeIdle > 0 {
					_ = controller.SetWriteDeadline(time.Now().Add(writeIdle))
				}
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

type idleReadBody struct {
	io.ReadCloser
	controller *http.ResponseController
	idle       time.Duration
	req        *http.Request
	w          http.ResponseWriter
}

func (b *idleReadBody) Read(p []byte) (int, error) {
	if err := b.controller.SetReadDeadline(time.Now().Add(b.idle)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	n, err := b.ReadCloser.Read(p)
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() && b.req.ProtoMajor == 1 {
		b.req.Close = true
		b.w.Header().Set("Connection", "close")
	}
	// Leave a failed read deadline in place until handler return; no unbounded H1 drain.
	if err == nil || err == io.EOF {
		_ = b.controller.SetReadDeadline(time.Time{})
	}
	return n, err
}

type idleResponseWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	idle       time.Duration
	hijacked   bool
	readIdle   time.Duration
	req        *http.Request
}

func (w *idleResponseWriter) armDrain() {
	if w.readIdle > 0 && w.req.ProtoMajor == 1 && w.req.Body != nil {
		_ = w.controller.SetReadDeadline(time.Now().Add(w.readIdle))
	}
}
func (w *idleResponseWriter) WriteHeader(status int) {
	w.armDrain()
	if w.idle > 0 {
		_ = w.controller.SetWriteDeadline(time.Now().Add(w.idle))
		defer func() { _ = w.controller.SetWriteDeadline(time.Time{}) }()
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *idleResponseWriter) CloseNotify() <-chan bool {
	if n, ok := w.ResponseWriter.(interface{ CloseNotify() <-chan bool }); ok {
		return n.CloseNotify()
	}
	return make(chan bool)
}

func (w *idleResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *idleResponseWriter) Write(p []byte) (int, error) {
	w.armDrain()
	if w.idle > 0 {
		if err := w.controller.SetWriteDeadline(time.Now().Add(w.idle)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return 0, err
		}
		defer func() { _ = w.controller.SetWriteDeadline(time.Time{}) }()
	}
	return w.ResponseWriter.Write(p)
}
func (w *idleResponseWriter) FlushError() error {
	w.armDrain()
	if w.idle > 0 {
		if err := w.controller.SetWriteDeadline(time.Now().Add(w.idle)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		defer func() { _ = w.controller.SetWriteDeadline(time.Time{}) }()
	}
	return w.controller.Flush()
}
func (w *idleResponseWriter) Flush() { _ = w.FlushError() }
func (w *idleResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	_ = w.controller.SetReadDeadline(time.Time{})
	_ = w.controller.SetWriteDeadline(time.Time{})
	conn, rw, err := w.controller.Hijack()
	if err == nil {
		w.hijacked = true
	}
	return conn, rw, err
}
func (w *idleResponseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
