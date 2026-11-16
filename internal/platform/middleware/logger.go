package middleware

import (
	"io"
	"log/slog"
	"net/http"
	"time"
)

// NewLogger returns a JSON slog logger that redacts phone numbers and credentials (see RedactingHandler).
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(NewRedactingHandler(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})))
}

// LoggerOptions tunes Logger.
type LoggerOptions struct {
	// Resolver resolves the client IP when RealIP did not run earlier in the chain. Nil trusts no proxy.
	Resolver *IPResolver
	// Skip suppresses the log line (e.g. the Docker health probe every 15 s).
	Skip func(r *http.Request) bool
}

// Logger writes one access-log line per request: method, path (never the query string, which may carry a
// token), route, status, duration, response size, request id, user id and client IP. 5xx log at error level,
// 4xx at warn, everything else at info. Put Recover inside Logger so a panic is logged with its 500. The logger's handler is wrapped in RedactingHandler unless it already
// is one, so phone numbers and tokens are always masked.
func Logger(log *slog.Logger, opts LoggerOptions) func(http.Handler) http.Handler {
	if _, ok := log.Handler().(*RedactingHandler); !ok {
		log = slog.New(NewRedactingHandler(log.Handler()))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r, in := withInfo(r)
			if in.get(func(i *Info) string { return i.clientIP }) == "" {
				setClientIP(in, opts.Resolver.ClientIP(r))
			}
			start := time.Now()
			rec := newRecorder(w)
			defer func() {
				if opts.Skip != nil && opts.Skip(r) {
					return
				}
				status := rec.Status()
				level := slog.LevelInfo
				switch {
				case status >= 500:
					level = slog.LevelError
				case status >= 400:
					level = slog.LevelWarn
				}
				route := RouteFrom(r.Context())
				if route == "" {
					route = r.Pattern
				}
				log.LogAttrs(r.Context(), level, "http",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("route", route),
					slog.Int("status", status),
					slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
					slog.Int64("bytes", rec.bytes),
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("user_id", UserIDFrom(r.Context())),
					slog.String("ip", ClientIPFrom(r.Context())),
				)
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
