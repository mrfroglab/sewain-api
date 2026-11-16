package middleware

import (
	"context"
	"net/http"
	"sync"
)

// Info is the per-request record shared by the middleware chain. Inner layers (auth, the router) fill it in
// and outer layers (the access logger) read it after the handler returns, so it is a mutable holder stored
// once in the request context instead of a chain of context values.
type Info struct {
	mu        sync.RWMutex
	requestID string
	userID    string
	clientIP  string
	route     string
}

type infoKey struct{}

// withInfo returns the request carrying an Info holder, adding one if it is missing.
func withInfo(r *http.Request) (*http.Request, *Info) {
	if in, ok := r.Context().Value(infoKey{}).(*Info); ok {
		return r, in
	}
	in := &Info{}
	return r.WithContext(context.WithValue(r.Context(), infoKey{}, in)), in
}

func infoFrom(ctx context.Context) *Info {
	in, _ := ctx.Value(infoKey{}).(*Info)
	return in
}

// WithInfo attaches an empty Info holder to ctx. Useful in tests and for code paths outside the HTTP chain.
func WithInfo(ctx context.Context) context.Context {
	if infoFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, infoKey{}, &Info{})
}

func (in *Info) get(f func(*Info) string) string {
	if in == nil {
		return ""
	}
	in.mu.RLock()
	defer in.mu.RUnlock()
	return f(in)
}

func (in *Info) set(f func(*Info)) {
	if in == nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	f(in)
}

// RequestIDFrom returns the request ID set by RequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	return infoFrom(ctx).get(func(i *Info) string { return i.requestID })
}

// UserIDFrom returns the authenticated user ID recorded with SetUserID, or "".
func UserIDFrom(ctx context.Context) string {
	return infoFrom(ctx).get(func(i *Info) string { return i.userID })
}

// ClientIPFrom returns the client IP resolved by RealIP (or the logger), or "".
func ClientIPFrom(ctx context.Context) string {
	return infoFrom(ctx).get(func(i *Info) string { return i.clientIP })
}

// RouteFrom returns the http.ServeMux pattern recorded with SetRoute, or "".
func RouteFrom(ctx context.Context) string {
	return infoFrom(ctx).get(func(i *Info) string { return i.route })
}

// SetUserID records the authenticated user for logs and per-user rate limits. The auth layer calls it once
// it has resolved the session. It is a no-op when the request did not pass through this package's chain.
func SetUserID(ctx context.Context, userID string) {
	infoFrom(ctx).set(func(i *Info) { i.userID = userID })
}

// SetRoute records the matched route pattern (r.Pattern) so outer layers can label logs and metrics.
func SetRoute(ctx context.Context, pattern string) {
	infoFrom(ctx).set(func(i *Info) { i.route = pattern })
}

func setRequestID(in *Info, id string) { in.set(func(i *Info) { i.requestID = id }) }
func setClientIP(in *Info, ip string)  { in.set(func(i *Info) { i.clientIP = ip }) }

// Chain wraps h with mws so that mws[0] is the outermost layer: Chain(h, a, b) == a(b(h)).
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
