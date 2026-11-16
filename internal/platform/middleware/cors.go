package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORSOptions configures CORS. Origins are compared exactly (scheme://host[:port], no trailing slash).
type CORSOptions struct {
	AllowedOrigins   []string
	AllowCredentials bool // needed for the session cookie (ROADMAP D6)
	AllowedMethods   []string
	AllowedHeaders   []string
	ExposedHeaders   []string
	MaxAge           time.Duration // preflight cache
}

// DefaultCORS returns the settings the API needs for the given origins.
func DefaultCORS(origins []string) CORSOptions {
	return CORSOptions{
		AllowedOrigins:   origins,
		AllowCredentials: true,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "Idempotency-Key", RequestIDHeader},
		ExposedHeaders:   []string{RequestIDHeader, "Retry-After", "ETag", "X-Content-SHA256"},
		MaxAge:           10 * time.Minute,
	}
}

// CORSForbiddenMessage is the body of a rejected preflight.
const CORSForbiddenMessage = "Origin tidak diizinkan."

// CORS implements a strict allow-list. A wildcard "*" is honored only without credentials, since browsers
// refuse "*" together with cookies. Preflights (OPTIONS + Access-Control-Request-Method) from allowed origins
// get 204 with the cached policy; from other origins 403. Simple requests from other origins pass through
// without CORS headers, so the browser blocks the response.
func CORS(o CORSOptions) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(o.AllowedOrigins))
	wildcard := false
	for _, a := range o.AllowedOrigins {
		a = strings.TrimRight(strings.TrimSpace(a), "/")
		if a == "*" {
			wildcard = !o.AllowCredentials
			continue
		}
		if a != "" {
			allowed[a] = true
		}
	}
	methods := strings.Join(o.AllowedMethods, ", ")
	headers := strings.Join(o.AllowedHeaders, ", ")
	exposed := strings.Join(o.ExposedHeaders, ", ")
	maxAge := strconv.Itoa(int(o.MaxAge.Seconds()))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
			h := w.Header()
			h.Add("Vary", "Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			ok := allowed[origin] || wildcard
			if !ok {
				if preflight {
					WriteError(w, http.StatusForbidden, CORSForbiddenMessage)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if allowed[origin] {
				h.Set("Access-Control-Allow-Origin", origin)
			} else {
				h.Set("Access-Control-Allow-Origin", "*")
			}
			if o.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if preflight {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", methods)
				h.Set("Access-Control-Allow-Headers", headers)
				if o.MaxAge > 0 {
					h.Set("Access-Control-Max-Age", maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if exposed != "" {
				h.Set("Access-Control-Expose-Headers", exposed)
			}
			next.ServeHTTP(w, r)
		})
	}
}
