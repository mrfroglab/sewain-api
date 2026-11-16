// Package middleware holds the HTTP middleware shared by the Sewain API: request IDs, structured access logs
// with PII masking, panic recovery, body limits, timeouts, security headers and CORS.
//
// Every middleware has the shape func(http.Handler) http.Handler and can be combined with Chain. The
// recommended order is documented in docs/integration/ops.md.
package middleware
