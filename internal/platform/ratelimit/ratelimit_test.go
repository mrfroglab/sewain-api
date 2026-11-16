package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)} }

func TestBurstThenDeny(t *testing.T) {
	clk := newClock()
	m := NewMemory(MemoryOptions{Now: clk.Now})
	rate := Rate{Limit: 60, Per: time.Minute, Burst: 3}
	ctx := context.Background()
	for i := range 3 {
		d, _ := m.Allow(ctx, "k", rate)
		if !d.Allowed || d.Remaining != 2-i {
			t.Fatalf("request %d: %+v", i, d)
		}
	}
	d, _ := m.Allow(ctx, "k", rate)
	if d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("4th request: %+v, want denied with RetryAfter 1s", d)
	}
	// Other keys have their own bucket.
	if d, _ := m.Allow(ctx, "other", rate); !d.Allowed {
		t.Fatal("independent key denied")
	}
}

func TestRefill(t *testing.T) {
	clk := newClock()
	m := NewMemory(MemoryOptions{Now: clk.Now})
	rate := Rate{Limit: 10, Per: time.Minute, Burst: 2} // one token every 6 s
	ctx := context.Background()
	m.Allow(ctx, "k", rate)
	m.Allow(ctx, "k", rate)
	if d, _ := m.Allow(ctx, "k", rate); d.Allowed || d.RetryAfter != 6*time.Second {
		t.Fatalf("empty bucket: %+v", d)
	}
	clk.Advance(3 * time.Second)
	if d, _ := m.Allow(ctx, "k", rate); d.Allowed || d.RetryAfter != 3*time.Second {
		t.Fatalf("half refilled: %+v", d)
	}
	clk.Advance(3 * time.Second)
	if d, _ := m.Allow(ctx, "k", rate); !d.Allowed {
		t.Fatalf("refilled token denied: %+v", d)
	}
	clk.Advance(time.Hour) // never exceeds burst
	m.Allow(ctx, "k", rate)
	m.Allow(ctx, "k", rate)
	if d, _ := m.Allow(ctx, "k", rate); d.Allowed {
		t.Fatal("bucket refilled beyond burst")
	}
}

func TestSweepDropsIdleBuckets(t *testing.T) {
	clk := newClock()
	m := NewMemory(MemoryOptions{Now: clk.Now, IdleTTL: time.Minute})
	ctx := context.Background()
	m.Allow(ctx, "old", Public.Rate)
	clk.Advance(30 * time.Second)
	m.Allow(ctx, "new", Public.Rate)
	clk.Advance(30 * time.Second)
	if n := m.Sweep(); n != 1 || m.Len() != 1 {
		t.Fatalf("swept %d, %d left; want 1 and 1", n, m.Len())
	}
	clk.Advance(time.Minute)
	if n := m.Sweep(); n != 1 || m.Len() != 0 {
		t.Fatalf("swept %d, %d left", n, m.Len())
	}
}

func TestJanitorSweeps(t *testing.T) {
	clk := newClock()
	m := NewMemory(MemoryOptions{Now: clk.Now, IdleTTL: time.Second})
	m.Allow(context.Background(), "k", Public.Rate)
	clk.Advance(2 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.StartJanitor(ctx, 5*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for m.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("janitor did not sweep")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func serve(h http.Handler, ip, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
	req.RemoteAddr = ip + ":1234"
	req = req.WithContext(middleware.WithInfo(req.Context()))
	if user != "" {
		middleware.SetUserID(req.Context(), user)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestMiddleware429(t *testing.T) {
	clk := newClock()
	m := NewMemory(MemoryOptions{Now: clk.Now})
	var limited []string
	h := Middleware(m, Auth, Options{OnLimited: func(p string, _ *http.Request) { limited = append(limited, p) }})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	for i := range Auth.Rate.Burst {
		if rr := serve(h, "203.0.113.1", ""); rr.Code != 200 {
			t.Fatalf("request %d: %d", i, rr.Code)
		}
	}
	rr := serve(h, "203.0.113.1", "")
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d", rr.Code)
	}
	if got := rr.Header().Get("Retry-After"); got != "6" { // 10/min → one token every 6 s
		t.Errorf("Retry-After %q", got)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	if body["ok"] != false || body["error"] != "Terlalu banyak permintaan. Coba lagi dalam 6 detik." {
		t.Errorf("body %v", body)
	}
	if len(limited) != 1 || limited[0] != "auth" {
		t.Errorf("OnLimited calls %v", limited)
	}
	// A different IP is unaffected; after the refill the first IP is allowed again.
	if rr := serve(h, "203.0.113.2", ""); rr.Code != 200 {
		t.Errorf("other IP: %d", rr.Code)
	}
	clk.Advance(6 * time.Second)
	if rr := serve(h, "203.0.113.1", ""); rr.Code != 200 {
		t.Errorf("after refill: %d", rr.Code)
	}
}

func TestByUserFallsBackToIP(t *testing.T) {
	m := NewMemory(MemoryOptions{})
	p := Profile{Name: "t", Rate: Rate{Limit: 1, Per: time.Hour, Burst: 1}, Key: ByUser}
	h := Middleware(m, p, Options{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// Same user from two IPs shares a bucket.
	if serve(h, "10.0.0.1", "u-1").Code != 200 || serve(h, "10.0.0.2", "u-1").Code != 429 {
		t.Fatal("user bucket not shared across IPs")
	}
	// Guests are limited per IP.
	if serve(h, "10.0.0.3", "").Code != 200 || serve(h, "10.0.0.3", "").Code != 429 {
		t.Fatal("guest not limited by IP")
	}
}

type failing struct{}

func (failing) Allow(context.Context, string, Rate) (Decision, error) {
	return Decision{}, errors.New("redis down")
}

func TestMiddlewareFailsOpen(t *testing.T) {
	h := Middleware(failing{}, Write, Options{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if rr := serve(h, "10.0.0.1", "u-1"); rr.Code != 200 {
		t.Fatalf("limiter error should fail open, got %d", rr.Code)
	}
}

func TestProfiles(t *testing.T) {
	want := map[string]int{"public": 120, "write": 60, "upload": 20, "auth": 10}
	for _, p := range Profiles() {
		if p.Rate.Limit != want[p.Name] || p.Rate.Per != time.Minute || p.Key == nil {
			t.Errorf("profile %s: %+v", p.Name, p.Rate)
		}
	}
}

// Finding P1: ByIP keyed on the full address. One IPv6 customer gets a whole /64 (2^64 addresses), so an
// attacker rotating the low bits got a fresh bucket per request: the auth profile (OTP) was unlimited and
// the in-memory limiter grew one bucket per address. IPv6 clients are now keyed by their /64.
func TestByIPGroupsIPv6By64(t *testing.T) {
	key := func(ip string) string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = net.JoinHostPort(ip, "1234")
		var got string
		middleware.Chain(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = ByIP(r) }),
			middleware.RealIP(nil)).ServeHTTP(httptest.NewRecorder(), r)
		return got
	}
	a, b := key("2001:db8:1:2::1"), key("2001:db8:1:2:ffff:ffff:ffff:fffe")
	if a != b {
		t.Fatalf("same /64 gave different keys: %q vs %q", a, b)
	}
	if c := key("2001:db8:1:3::1"); c == a {
		t.Fatalf("different /64 shared a key: %q", c)
	}
	if v4a, v4b := key("203.0.113.7"), key("203.0.113.8"); v4a == v4b || v4a != "ip:203.0.113.7" {
		t.Fatalf("IPv4 keys = %q, %q; want one per address", v4a, v4b)
	}
	if m := key("::ffff:203.0.113.7"); m != "ip:203.0.113.7" {
		t.Fatalf("IPv4-mapped key = %q", m)
	}

	// End to end: the auth profile must stop a client that rotates addresses inside its /64.
	m := NewMemory(MemoryOptions{Now: newClock().Now})
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }),
		middleware.RealIP(nil), Middleware(m, Auth, Options{}))
	denied := 0
	for i := range 50 {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/otp/request", nil)
		r.RemoteAddr = fmt.Sprintf("[2001:db8:1:2::%x]:443", i+1)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusTooManyRequests {
			denied++
		}
	}
	if denied != 50-Auth.Rate.Burst {
		t.Fatalf("denied %d of 50 rotating-IPv6 requests, want %d", denied, 50-Auth.Rate.Burst)
	}
}

func TestIPBucket(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":            "203.0.113.7",
		"::ffff:203.0.113.7":     "203.0.113.7",
		"2001:db8:a:b:c:d:e:f":   "2001:db8:a:b::/64",
		"[2001:db8::1]":          "[2001:db8::1]", // not an address: returned unchanged
		"":                       "",
		"fe80::1%eth0":           "fe80::/64",
		"2001:db8:a:b::1/128 xx": "2001:db8:a:b::1/128 xx",
	} {
		if got := IPBucket(in); got != want {
			t.Errorf("IPBucket(%q) = %q, want %q", in, got, want)
		}
	}
}
