package middleware

import (
	"net/http"
	"strings"
)

// SecurityHeaders sets the API's baseline response headers:
//   - X-Content-Type-Options: nosniff
//   - Referrer-Policy: no-referrer
//   - Cross-Origin-Resource-Policy: same-site (the web app on the same site can still embed photos)
//   - X-Frame-Options: DENY
//   - Cache-Control: no-store on JSON responses that did not set their own Cache-Control, because API JSON is
//     private to the viewer (snapshot, profile, bookings).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-site")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(&cacheWriter{recorder: recorder{ResponseWriter: w}}, r)
	})
}

type cacheWriter struct{ recorder }

func (w *cacheWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		h := w.Header()
		if h.Get("Cache-Control") == "" && strings.HasPrefix(h.Get("Content-Type"), "application/json") {
			h.Set("Cache-Control", "no-store")
		}
	}
	w.recorder.WriteHeader(code)
}

func (w *cacheWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.recorder.Write(b)
}
