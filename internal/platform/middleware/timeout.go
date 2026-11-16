package middleware

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TimeoutMessage is the body text when a request runs past its deadline.
const TimeoutMessage = "Server sedang sibuk, coba lagi sebentar lagi."

// TimeoutOptions tunes Timeout.
type TimeoutOptions struct {
	// Skip exempts requests from the deadline. WebSocket upgrades are always exempt.
	Skip func(r *http.Request) bool
}

// Timeout gives each request a deadline. The handler's context is cancelled at d and, if it has not
// finished, the client receives 503 {"ok":false,"error":TimeoutMessage}. Like http.TimeoutHandler the
// response is buffered until the handler returns, so streaming handlers (WebSocket, SSE, large downloads)
// must be skipped. A panic in the handler is re-raised on the serving goroutine so Recover still sees it.
func Timeout(d time.Duration, opts TimeoutOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if IsWebSocket(r) || (opts.Skip != nil && opts.Skip(r)) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			r = r.WithContext(ctx)

			tw := &timeoutWriter{h: make(http.Header)}
			done := make(chan struct{})
			panicked := make(chan any, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						panicked <- p
					}
				}()
				next.ServeHTTP(tw, r)
				close(done)
			}()
			select {
			case p := <-panicked:
				panic(p)
			case <-done:
				tw.mu.Lock()
				defer tw.mu.Unlock()
				dst := w.Header()
				for k, vv := range tw.h {
					dst[k] = vv
				}
				if tw.code == 0 {
					tw.code = http.StatusOK
				}
				w.WriteHeader(tw.code)
				_, _ = w.Write(tw.buf.Bytes())
			case <-ctx.Done():
				tw.mu.Lock()
				defer tw.mu.Unlock()
				tw.timedOut = true
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					WriteError(w, http.StatusServiceUnavailable, TimeoutMessage)
				}
				// Otherwise the client went away; nothing useful to write.
			}
		})
	}
}

// IsWebSocket reports whether r asks for a WebSocket upgrade.
func IsWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

type timeoutWriter struct {
	mu       sync.Mutex
	h        http.Header
	buf      bytes.Buffer
	code     int
	timedOut bool
}

func (tw *timeoutWriter) Header() http.Header { return tw.h }

func (tw *timeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	if tw.code == 0 {
		tw.code = http.StatusOK
	}
	return tw.buf.Write(p)
}

func (tw *timeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut || tw.code != 0 {
		return
	}
	tw.code = code
}
