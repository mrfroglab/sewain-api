package middleware

import (
	"encoding/json"
	"net/http"
)

// recorder captures the status code and body size. It exposes Unwrap so http.ResponseController and
// coder/websocket can still reach the underlying Hijacker/Flusher.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func newRecorder(w http.ResponseWriter) *recorder { return &recorder{ResponseWriter: w} }

func (w *recorder) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		// 1xx responses are informational; the final status is still to come.
		if code >= 200 || code == http.StatusSwitchingProtocols {
			w.wroteHeader = true
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *recorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Status returns the response status, 200 if the handler wrote nothing, or 0 if nothing was written at all.
func (w *recorder) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// WriteError writes the API's standard error body: {"ok":false,"error":msg}.
func WriteError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}
