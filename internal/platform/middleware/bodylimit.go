package middleware

import (
	"errors"
	"net/http"
	"strings"
)

// Body size limits (F5.1).
const (
	DefaultBodyLimit int64 = 1 << 20  // JSON actions and everything else
	UploadBodyLimit  int64 = 12 << 20 // multipart photo / KYC uploads
)

// TooLargeMessage is the 413 body text.
const TooLargeMessage = "Ukuran data terlalu besar."

// BodyRule overrides the default limit for requests whose method matches (empty = any) and whose path
// equals Path or, when Path ends in "/", starts with it.
type BodyRule struct {
	Method string
	Path   string
	Limit  int64
}

// BodyLimit caps request bodies. A declared Content-Length above the limit is rejected with 413 before the
// handler runs; chunked bodies are wrapped in http.MaxBytesReader, so a handler that hits the limit while
// reading gets an error for which IsBodyTooLarge reports true (respond with RespondTooLarge).
//
// The first matching rule wins. Per-route limits must be decided here, in the outer layer, because an inner
// MaxBytesReader cannot raise a limit set by an outer one.
func BodyLimit(def int64, rules ...BodyRule) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit := def
			for _, rule := range rules {
				if rule.matches(r) {
					limit = rule.Limit
					break
				}
			}
			if limit > 0 && r.Body != nil && r.Body != http.NoBody {
				if r.ContentLength > limit {
					RespondTooLarge(w)
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (b BodyRule) matches(r *http.Request) bool {
	if b.Method != "" && !strings.EqualFold(b.Method, r.Method) {
		return false
	}
	if strings.HasSuffix(b.Path, "/") {
		return strings.HasPrefix(r.URL.Path, b.Path)
	}
	return r.URL.Path == b.Path
}

// IsBodyTooLarge reports whether err (from reading or parsing the body) was caused by the body limit.
func IsBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// RespondTooLarge writes the standard 413 JSON error.
func RespondTooLarge(w http.ResponseWriter) {
	w.Header().Set("Connection", "close")
	WriteError(w, http.StatusRequestEntityTooLarge, TooLargeMessage)
}
