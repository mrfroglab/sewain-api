package middleware

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// IPResolver finds the client IP. Proxy headers (CF-Connecting-IP, X-Forwarded-For) are honoured only when
// the direct peer is inside one of the trusted CIDRs; otherwise anyone could spoof their IP and dodge
// per-IP rate limits.
type IPResolver struct {
	trusted []netip.Prefix
}

// NewIPResolver builds a resolver from CIDRs or bare IPs (e.g. "172.16.0.0/12,127.0.0.1"). An empty list
// means no proxy is trusted and RemoteAddr is always used.
func NewIPResolver(cidrs []string) (*IPResolver, error) {
	res := &IPResolver{}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			a, err := netip.ParseAddr(c)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", c, err)
			}
			res.trusted = append(res.trusted, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", c, err)
		}
		res.trusted = append(res.trusted, p.Masked())
	}
	return res, nil
}

// ParseCIDRList splits a comma-separated env value such as TRUSTED_PROXY_CIDRS.
func ParseCIDRList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (res *IPResolver) isTrusted(a netip.Addr) bool {
	if res == nil {
		return false
	}
	a = a.Unmap()
	for _, p := range res.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP returns the best-known client IP for r.
func (res *IPResolver) ClientIP(r *http.Request) string {
	peer := peerAddr(r.RemoteAddr)
	if !peer.IsValid() {
		return r.RemoteAddr
	}
	if !res.isTrusted(peer) {
		return peer.String()
	}
	// Cloudflare sets CF-Connecting-IP to the real visitor; cloudflared forwards it to Caddy and Caddy to us.
	if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
		return a.Unmap().String()
	}
	// Walk X-Forwarded-For from the right, skipping our own proxies; the first untrusted hop is the client.
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !res.isTrusted(a) || i == 0 {
			return a.Unmap().String()
		}
	}
	return peer.String()
}

func peerAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// RealIP resolves the client IP once and stores it in the request context (ClientIPFrom). Place it before
// the logger and the rate limiter.
func RealIP(res *IPResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r, in := withInfo(r)
			setClientIP(in, res.ClientIP(r))
			next.ServeHTTP(w, r)
		})
	}
}
