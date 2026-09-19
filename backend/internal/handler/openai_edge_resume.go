package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

const edgeStreamSessionHeader = "X-Sub2API-Edge-Stream-Session"
const edgeStreamBindingHeader = "X-Sub2API-Edge-Stream-Binding"
const edgeStreamOffsetHeader = "X-Sub2API-Edge-Stream-Offset"

type edgeStreamSession struct {
	url, id, binding, secret string
	request                  *http.Request
	body                     []byte
}

func doEdgeSessionRequest(req *http.Request) (*http.Response, error) {
	client := *openAIEdgeIngressClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(req)
}

// Reserve before execution. Reservation failures are safe to fall back from:
// this endpoint cannot send a generation request. A missing recovery session
// after execution is NEVER permission to execute a fresh request.
func reserveEdgeStreamSession(req *http.Request, body []byte, addr, secret string) (*edgeStreamSession, bool) {
	id := uuid.NewString()
	hash := sha256.New()
	for _, value := range []string{req.Method, req.URL.RequestURI(), req.Header.Get("Authorization")} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(body)
	s := &edgeStreamSession{url: openAIEdgeIngressURL(addr, "/internal/edge/stream-session/"+id), id: id,
		binding: hex.EncodeToString(hash.Sum(nil)), secret: secret, request: req, body: body}
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	reservation, _ := http.NewRequestWithContext(ctx, http.MethodPut, s.url, nil)
	s.authenticate(reservation)
	resp, err := doEdgeSessionRequest(reservation)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, true // Old Edge binary: retain the established transport.
	}
	if resp.StatusCode != http.StatusNoContent {
		return nil, false
	}
	s.authenticate(req)
	req.Header.Set(edgeStreamSessionHeader, id)
	return s, true
}

func (s *edgeStreamSession) authenticate(req *http.Request) {
	req.Header.Set(openAIEdgeSecretHeader, s.secret)
	req.Header.Set(edgeStreamBindingHeader, s.binding)
}

func (s *edgeStreamSession) start(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	// This only breaks the internal socket. The detached Edge execution and
	// downstream context remain alive, including a slow upstream header wait.
	// Match Edge's upload budget; start the shorter header timer only after
	// the full POST has been written, including large context/tool payloads.
	timer := time.AfterFunc(300*time.Second, cancel)
	var timerMu sync.Mutex
	headersDone := false
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		timerMu.Lock()
		defer timerMu.Unlock()
		if !headersDone && info.Err == nil {
			timer.Reset(15 * time.Second)
		}
	}})
	resp, err := doEdgeSessionRequest(req.WithContext(ctx))
	timerMu.Lock()
	headersDone = true
	timer.Stop()
	timerMu.Unlock()
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &edgeCancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func (s *edgeStreamSession) close() {
	// Best effort explicit cancellation; Edge also retires detached sessions.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, s.url, nil)
	s.authenticate(req)
	if resp, err := doEdgeSessionRequest(req); err == nil {
		_ = resp.Body.Close()
	}
}

func (s *edgeStreamSession) resume(offset int64) (*http.Response, error) {
	ctx, cancel := context.WithCancel(s.request.Context())
	timer := time.AfterFunc(25*time.Second, cancel)
	defer timer.Stop()
	// Only bound reconnect/header acquisition. The successful body inherits
	// the original downstream context and must not acquire a 25s lifetime.
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(s.request.Context(), http.MethodGet, s.url, nil)
		s.authenticate(req)
		req.Header.Set(edgeStreamOffsetHeader, strconv.FormatInt(offset, 10))
		attemptCtx, attemptCancel := context.WithCancel(req.Context())
		stop := context.AfterFunc(ctx, attemptCancel)
		resp, err := doEdgeSessionRequest(req.WithContext(attemptCtx))
		stop()
		if err == nil && resp.StatusCode == http.StatusTooEarly {
			attemptCancel()
			_ = resp.Body.Close()
			if offset != 0 {
				return nil, errors.New("edge session lost execution state")
			}
			// Atomic start in the reserved session makes this safe even when a
			// delayed original POST arrives concurrently. It cannot run twice.
			retry := s.request.Clone(attemptCtx)
			// Use a fresh context: the GET attempt has just been closed.
			attemptCtx, attemptCancel = context.WithCancel(s.request.Context())
			retry = retry.WithContext(attemptCtx)
			retry.Body = io.NopCloser(bytes.NewReader(s.body))
			retry.GetBody = nil
			stop = context.AfterFunc(ctx, attemptCancel)
			resp, err = doEdgeSessionRequest(retry)
			stop()
		}
		if err == nil {
			if resp.StatusCode == http.StatusAccepted && resp.Header.Get(edgeStreamSessionHeader) == "pending" {
				// Edge confirms the original execution is alive. Its existing
				// upstream budget still owns the wait; don't impose a new 25s
				// generation timeout just because this transport reconnected.
				timer.Reset(25 * time.Second)
			}
			if resp.StatusCode == http.StatusNoContent {
				attemptCancel()
				_ = resp.Body.Close()
				return nil, io.EOF
			}
			if resp.Header.Get(edgeStreamSessionHeader) == "v1" && resp.Header.Get(edgeStreamOffsetHeader) == strconv.FormatInt(offset, 10) {
				resp.Body = &edgeCancelBody{ReadCloser: resp.Body, cancel: attemptCancel}
				return resp, nil
			}
			attemptCancel()
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
				attemptCancel()
				return nil, errors.New("edge recovery session unavailable")
			}
		}
		attemptCancel()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type edgeCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *edgeCancelBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

func (b *edgeCancelBody) Read(p []byte) (int, error) {
	// Edge's stream heartbeat interval is 20s. An internal socket that stops
	// delivering bytes can be reattached without stopping model generation.
	timer := time.AfterFunc(35*time.Second, b.cancel)
	defer timer.Stop()
	return b.ReadCloser.Read(p)
}

type edgeResumingBody struct {
	io.ReadCloser
	session         *edgeStreamSession
	offset          int64
	terminal        openAIEdgeTerminalScanner
	noProgressSince time.Time
}

func (b *edgeResumingBody) Read(p []byte) (int, error) {
	for {
		n, err := b.ReadCloser.Read(p)
		if n > 0 {
			b.noProgressSince = time.Time{}
			b.offset += int64(n)
			b.terminal.feed(p[:n])
			// io.Reader can return bytes and EOF together. Revisit EOF on the
			// next read after these exact bytes have reached the relay.
			return n, nil
		}
		if err == nil || b.terminal.seen || b.session.request.Context().Err() != nil {
			return n, err
		}
		_ = b.ReadCloser.Close()
		if b.noProgressSince.IsZero() {
			b.noProgressSince = time.Now()
		} else {
			if time.Since(b.noProgressSince) >= 25*time.Second {
				return 0, errors.New("edge recovery made no progress")
			}
			select {
			case <-b.session.request.Context().Done():
				return 0, b.session.request.Context().Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		resp, resumeErr := b.session.resume(b.offset)
		if resumeErr != nil {
			return 0, resumeErr
		}
		if resp.StatusCode >= 400 {
			_ = resp.Body.Close()
			return 0, errors.New("edge stream recovery failed")
		}
		b.ReadCloser = resp.Body
	}
}
