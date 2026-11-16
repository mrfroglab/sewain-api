package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// RequestIDHeader is read from incoming requests and always set on responses.
const RequestIDHeader = "X-Request-ID"

// RequestID accepts a well-formed incoming X-Request-ID (so a proxy's ID survives) or generates a new one,
// stores it in the request context and echoes it in the response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, in := withInfo(r)
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = NewRequestID()
		}
		setRequestID(in, id)
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r)
	})
}

// NewRequestID returns 16 random bytes as 32 hex characters.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// validRequestID allows 8–128 characters from a conservative set, so a client cannot inject log noise,
// header splitting or overly long values through the ID.
func validRequestID(id string) bool {
	if len(id) < 8 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}
