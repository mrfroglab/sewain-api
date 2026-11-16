package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
)

// Finding A3: ClientIP believed CLIENT_IP_HEADER from any peer and, for X-Forwarded-For, took the first
// (client-written) hop. A client sending "X-Forwarded-For: <random>" got a fresh OTP per-IP bucket on
// every request. The IP resolved by middleware.RealIP (which only trusts TRUSTED_PROXY_CIDRS) now wins,
// and an X-Forwarded-For fallback uses the hop appended by our own proxy (the last one).
func TestClientIPIgnoresSpoofedHeaders(t *testing.T) {
	s := &Service{Config: Config{ClientIPHeader: "X-Forwarded-For"}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
	r.RemoteAddr = "172.20.0.5:5555" // Caddy
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	if got := s.ClientIP(r); got != "198.51.100.7" {
		t.Fatalf("ClientIP with X-Forwarded-For = %q, want the proxy-appended hop 198.51.100.7", got)
	}

	// Behind the platform chain, RealIP's answer is authoritative.
	res, err := middleware.NewIPResolver(nil) // trusts no proxy: the peer is the client
	if err != nil {
		t.Fatal(err)
	}
	var got string
	middleware.Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = s.ClientIP(r) }),
		middleware.RealIP(res)).ServeHTTP(httptest.NewRecorder(), r)
	if got != "172.20.0.5" {
		t.Fatalf("ClientIP behind RealIP = %q, want 172.20.0.5", got)
	}
}
