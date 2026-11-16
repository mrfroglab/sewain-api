package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// InternalErrorMessage is the body text for unexpected server errors.
const InternalErrorMessage = "Terjadi kesalahan di server."

// Recover turns a panic into a 500 {"ok":false,"error":"Terjadi kesalahan di server."} and logs it with the
// stack trace and request id. http.ErrAbortHandler is re-raised so net/http can abort the connection quietly.
// If the handler already started the response, the status can no longer change; the panic is still logged.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := newRecorder(w)
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if err, ok := p.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
				log.LogAttrs(r.Context(), slog.LevelError, "panic",
					slog.Any("panic", p),
					slog.String("stack", string(debug.Stack())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("request_id", RequestIDFrom(r.Context())),
				)
				if !rec.wroteHeader {
					WriteError(rec, http.StatusInternalServerError, InternalErrorMessage)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
