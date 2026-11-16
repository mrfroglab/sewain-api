package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// queryResolver authenticates test connections from ?u=<id>&role=<role>.
func queryResolver(r *http.Request) (string, string) {
	return r.URL.Query().Get("u"), r.URL.Query().Get("role")
}

func startHub(t *testing.T, opt Options) (*Hub, *httptest.Server) {
	t.Helper()
	if opt.Resolve == nil {
		opt.Resolve = queryResolver
	}
	h := NewHub(opt)
	srv := httptest.NewServer(h)
	t.Cleanup(func() { h.Close(); srv.Close() })
	return h, srv
}

type testConn struct {
	t *testing.T
	c *websocket.Conn
}

func dial(t *testing.T, srv *httptest.Server, query string, hdr http.Header) (*testConn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/events"
	if query != "" {
		u += "?" + query
	}
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, resp, err
	}
	tc := &testConn{t: t, c: c}
	t.Cleanup(func() { _ = c.CloseNow() })
	return tc, resp, nil
}

func mustDial(t *testing.T, srv *httptest.Server, query string) *testConn {
	t.Helper()
	c, _, err := dial(t, srv, query, nil)
	if err != nil {
		t.Fatalf("dial %q: %v", query, err)
	}
	if m := c.next(); m.Type != "hello" {
		t.Fatalf("first message = %+v, want hello", m)
	}
	return c
}

func (c *testConn) next() message {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.c.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var m message
	if err := json.Unmarshal(b, &m); err != nil {
		c.t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

// expect reads the next messages and checks they are "changed" with exactly these versions, in order.
func (c *testConn) expect(versions ...int64) {
	c.t.Helper()
	for _, v := range versions {
		m := c.next()
		if m.Type != "changed" || m.Version != v {
			c.t.Fatalf("got %+v, want changed v%d", m, v)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHubRoutesOnlyToAffectedConnections(t *testing.T) {
	h, srv := startHub(t, Options{})
	a := mustDial(t, srv, "u=u-a")
	b := mustDial(t, srv, "u=u-b")
	admin := mustDial(t, srv, "u=u-admin&role=admin")
	guest := mustDial(t, srv, "")
	waitFor(t, func() bool { return h.Count() == 4 })

	h.Publish(Event{Version: 1, Users: []string{"u-a"}})
	h.Publish(Event{Version: 2, Admins: true})
	h.Publish(Event{Version: 3, Users: []string{"u-b", "u-admin"}, Admins: true}) // admin listed twice: one message
	h.Publish(Event{Version: 4, Public: true})                                    // sentinel: everyone

	a.expect(1, 4) // never sees B's or admin-only events
	b.expect(3, 4) // never sees A's event
	admin.expect(2, 3, 4)
	guest.expect(4) // guests only get Public
}

func TestHubHelloCarriesCurrentVersion(t *testing.T) {
	_, srv := startHub(t, Options{CurrentVersion: func(context.Context) (int64, error) { return 42, nil }})
	c, _, err := dial(t, srv, "u=u-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := c.next(); m.Type != "hello" || m.Version != 42 {
		t.Fatalf("hello = %+v", m)
	}
}

func TestHubConnectionLimitPerUser(t *testing.T) {
	h, srv := startHub(t, Options{MaxConnsPerUser: 2})
	first := mustDial(t, srv, "u=u-a")
	mustDial(t, srv, "u=u-a")
	_, resp, err := dial(t, srv, "u=u-a", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third connection: err=%v resp=%v, want 429", err, resp)
	}
	mustDial(t, srv, "u=u-b") // other users are unaffected

	_ = first.c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return h.Count() == 2 })
	mustDial(t, srv, "u=u-a") // a slot is free again
}

func TestHubRejectsForeignOrigin(t *testing.T) {
	_, srv := startHub(t, Options{AllowedOrigins: []string{"https://sewain.hallojanu.me", "https://*.example.org"}})
	_, resp, err := dial(t, srv, "", http.Header{"Origin": {"https://evil.example"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: err=%v resp=%v, want 403", err, resp)
	}
	for _, o := range []string{"https://sewain.hallojanu.me", "https://staging.example.org"} {
		if _, _, err := dial(t, srv, "", http.Header{"Origin": {o}}); err != nil {
			t.Fatalf("origin %s: %v", o, err)
		}
	}
}

func TestHubDropsClientWithFullQueueWithoutBlocking(t *testing.T) {
	h := NewHub(Options{SendBuffer: 1})
	_, slowCtx, _ := h.register("u-slow", "")
	fast, fastCtx, _ := h.register("u-fast", "")
	done := make(chan struct{})
	go func() {
		for v := int64(1); v <= 3; v++ {
			h.Publish(Event{Version: v, Users: []string{"u-slow"}})
		}
		h.Publish(Event{Version: 4, Users: []string{"u-fast"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full client queue")
	}
	if slowCtx.Err() == nil {
		t.Fatal("slow client was not dropped")
	}
	if fastCtx.Err() != nil {
		t.Fatal("fast client was dropped")
	}
	if got := <-fast.send; !strings.Contains(string(got), `"version":4`) {
		t.Fatalf("fast client got %s", got)
	}
}

func TestHubNonReadingSocketDoesNotStallOthers(t *testing.T) {
	h, srv := startHub(t, Options{SendBuffer: 4, WriteTimeout: 200 * time.Millisecond})
	slow, _, err := dial(t, srv, "u=u-slow", nil) // never reads, not even the hello
	if err != nil {
		t.Fatal(err)
	}
	_ = slow
	fast := mustDial(t, srv, "u=u-fast")
	waitFor(t, func() bool { return h.Count() == 2 })

	big := Event{Users: []string{"u-slow"}}
	start := time.Now()
	for v := int64(1); v <= 5000; v++ {
		big.Version = v
		h.Publish(big)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("publishing to a stalled client took %v", d)
	}
	h.Publish(Event{Version: 5001, Users: []string{"u-fast"}})
	fast.expect(5001)
	waitFor(t, func() bool { return h.Count() == 1 }) // the stalled socket was cut
}

func TestHubCloseRefusesNewConnections(t *testing.T) {
	h, srv := startHub(t, Options{})
	mustDial(t, srv, "u=u-a")
	h.Close()
	waitFor(t, func() bool { return h.Count() == 0 })
	_, resp, err := dial(t, srv, "u=u-a", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("after Close: err=%v resp=%v", err, resp)
	}
}

// Finding R1: with the zero value (as wired in docs/integration/notify.md) guests were unlimited. Each
// connection pins two goroutines, a send queue and socket buffers, so one client (or a /64 of IPv6
// addresses) could open guest sockets until the 256 MB API container died. Guests are now capped by
// default; a negative MaxGuestConns restores "unlimited" explicitly.
func TestGuestConnectionsCappedByDefault(t *testing.T) {
	h := NewHub(Options{})
	for i := range DefaultMaxGuestConns {
		if c, _, st := h.register("", ""); c == nil {
			t.Fatalf("guest %d refused with %d", i, st)
		}
	}
	if c, _, st := h.register("", ""); c != nil || st != http.StatusTooManyRequests {
		t.Fatalf("guest beyond the default cap: client=%v status=%d, want 429", c != nil, st)
	}
	if c, _, _ := h.register("u-1", ""); c == nil {
		t.Fatal("signed-in users are capped separately")
	}
	u := NewHub(Options{MaxGuestConns: -1})
	for range DefaultMaxGuestConns + 1 {
		if c, _, _ := u.register("", ""); c == nil {
			t.Fatal("MaxGuestConns -1 must not cap")
		}
	}
}
