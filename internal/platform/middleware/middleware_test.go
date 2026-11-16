package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func decodeErr(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

func TestRequestIDGeneratedAndPropagated(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	got := rr.Header().Get(RequestIDHeader)
	if len(got) != 32 || got != seen {
		t.Fatalf("header %q, context %q: want the same 32-char id", got, seen)
	}
}

func TestRequestIDAcceptsValidIncoming(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = RequestIDFrom(r.Context()) }))
	for _, tc := range []struct {
		in   string
		keep bool
	}{
		{"cf-ray-8a1b2c3d4e5f-SIN", true},
		{"short", false},
		{"bad id with spaces", false},
		{"inject\r\nX-Evil: 1", false},
		{strings.Repeat("a", 129), false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(RequestIDHeader, tc.in)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if (seen == tc.in) != tc.keep {
			t.Errorf("incoming %q: got %q, keep=%v", tc.in, seen, tc.keep)
		}
		if rr.Header().Get(RequestIDHeader) != seen {
			t.Errorf("response header %q != context %q", rr.Header().Get(RequestIDHeader), seen)
		}
	}
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestLoggerFieldsAndMasking(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil)) // deliberately not redacting: Logger must wrap it
	res, _ := NewIPResolver([]string{"10.0.0.0/8"})
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetUserID(r.Context(), "u-123")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello")
	}), RequestID, Logger(log, LoggerOptions{Resolver: res}))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/081234567890?token=supersecret&x=1", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)

	lines := logLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d: %s", len(lines), buf.String())
	}
	l := lines[0]
	raw := buf.String()
	for _, secret := range []string{"supersecret", "081234567890"} {
		if strings.Contains(raw, secret) {
			t.Errorf("log leaks %q: %s", secret, raw)
		}
	}
	want := map[string]any{
		"method": "POST", "path": "/api/v1/users/081******890", "status": float64(201),
		"bytes": float64(5), "user_id": "u-123", "ip": "203.0.113.9", "level": "INFO",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("%s = %v, want %v", k, l[k], v)
		}
	}
	if id, _ := l["request_id"].(string); len(id) != 32 {
		t.Errorf("request_id = %v", l["request_id"])
	}
	if _, ok := l["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms missing: %v", l)
	}
}

func TestRedactingHandlerMasksAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelDebug).With("session_token", "abc123def")
	log.Info("otp sent to +62 812-3456-7890",
		"phone", "+6281234567890",
		"authorization", "Bearer eyJhbGciOi.payload.sig",
		"detail", "header was Bearer eyJxyz and url /x?otp=123456",
		slog.Group("user", "phone_number", "081298765432", "name", "Budi"),
		"err", errTest("lookup 6281311112222 failed"),
	)
	raw := buf.String()
	for _, leak := range []string{"abc123def", "3456-7890", "6281234567890", "eyJhbGciOi", "eyJxyz", "123456", "081298765432", "6281311112222"} {
		if strings.Contains(raw, leak) {
			t.Errorf("log leaks %q: %s", leak, raw)
		}
	}
	l := logLines(t, &buf)[0]
	if l["phone"] != "+6281******890" {
		t.Errorf("phone = %v", l["phone"])
	}
	if l["authorization"] != redacted || l["session_token"] != redacted {
		t.Errorf("secrets not redacted: %v", l)
	}
	if u, _ := l["user"].(map[string]any); u["name"] != "Budi" || u["phone_number"] != "081******432" {
		t.Errorf("group = %v", l["user"])
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestMaskPhone(t *testing.T) {
	for in, want := range map[string]string{
		"+6281234567890": "+6281******890",
		"081234567890":   "081******890",
		"12345":          "*****",
	} {
		if got := MaskPhone(in); got != want {
			t.Errorf("MaskPhone(%q) = %q, want %q", in, got, want)
		}
	}
	// Things that are not phone numbers stay readable.
	for _, s := range []string{"booking B-2024-0001", "took 1234567 ns", "id 8123456789"} {
		if got := RedactString(s); got != s {
			t.Errorf("RedactString(%q) = %q", s, got)
		}
	}
}

func TestRecoverReturnsJSON500(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, slog.LevelInfo)
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), RequestID, Logger(log, LoggerOptions{}), Recover(log))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type %q", ct)
	}
	m := decodeErr(t, rr.Body)
	if m["ok"] != false || m["error"] != InternalErrorMessage {
		t.Fatalf("body %v", m)
	}
	lines := logLines(t, &buf)
	if len(lines) != 2 {
		t.Fatalf("want panic + access lines, got %s", buf.String())
	}
	if lines[0]["msg"] != "panic" || !strings.Contains(lines[0]["stack"].(string), "goroutine") || lines[0]["request_id"] == "" {
		t.Errorf("panic log %v", lines[0])
	}
	if lines[1]["status"] != float64(500) || lines[1]["level"] != "ERROR" {
		t.Errorf("access log %v", lines[1])
	}
}

func TestRecoverReraisesAbortHandler(t *testing.T) {
	h := Recover(slog.New(slog.DiscardHandler))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want ErrAbortHandler", p)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestBodyLimit(t *testing.T) {
	readAll := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			if IsBodyTooLarge(err) {
				RespondTooLarge(w)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := BodyLimit(10, BodyRule{Method: http.MethodPost, Path: "/upload", Limit: 100})(readAll)

	send := func(path string, n int, chunked bool) int {
		var body io.Reader = bytes.NewReader(make([]byte, n))
		if chunked {
			body = io.MultiReader(body) // hides the length → ContentLength stays -1
		}
		req := httptest.NewRequest(http.MethodPost, path, body)
		if chunked {
			req.ContentLength = -1
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	cases := []struct {
		path    string
		n       int
		chunked bool
		want    int
	}{
		{"/x", 10, false, 200},
		{"/x", 11, false, 413},
		{"/x", 11, true, 413},
		{"/upload", 100, false, 200},
		{"/upload", 101, true, 413},
	}
	for _, c := range cases {
		if got := send(c.path, c.n, c.chunked); got != c.want {
			t.Errorf("%s %d bytes chunked=%v: status %d, want %d", c.path, c.n, c.chunked, got, c.want)
		}
	}
	if DefaultBodyLimit != 1<<20 || UploadBodyLimit != 12<<20 {
		t.Fatal("default limits changed")
	}
}

func TestBodyLimit413Body(t *testing.T) {
	h := BodyLimit(5)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("123456")))
	if rr.Code != http.StatusRequestEntityTooLarge || decodeErr(t, rr.Body)["error"] != TooLargeMessage {
		t.Fatalf("got %d %s", rr.Code, rr.Body)
	}
}

func TestTimeout(t *testing.T) {
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
			_, _ = io.WriteString(w, "late")
		case <-r.Context().Done():
		}
	})
	h := Timeout(30*time.Millisecond, TimeoutOptions{})(slow)
	rr := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusServiceUnavailable || decodeErr(t, rr.Body)["error"] != TimeoutMessage {
		t.Fatalf("got %d", rr.Code)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("timeout did not cut the request short")
	}

	fast := Timeout(time.Second, TimeoutOptions{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test", "1")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "ok")
	}))
	rr = httptest.NewRecorder()
	fast.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusAccepted || rr.Body.String() != "ok" || rr.Header().Get("X-Test") != "1" {
		t.Fatalf("fast handler: %d %q %v", rr.Code, rr.Body, rr.Header())
	}
}

func TestTimeoutSkipsWebSocket(t *testing.T) {
	h := Timeout(10*time.Millisecond, TimeoutOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); ok {
			t.Error("websocket request got a deadline")
		}
		if _, ok := w.(*timeoutWriter); ok {
			t.Error("websocket request got a buffering writer")
		}
		time.Sleep(30 * time.Millisecond)
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestTimeoutPropagatesPanic(t *testing.T) {
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("in handler") }),
		Recover(slog.New(slog.DiscardHandler)), Timeout(time.Second, TimeoutOptions{}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rr.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/photo" {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "private, max-age=3600")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = io.WriteString(w, "{}")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/json", nil))
	for k, v := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-site",
		"X-Frame-Options":              "DENY",
		"Cache-Control":                "no-store",
	} {
		if got := rr.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/photo", nil))
	if got := rr.Header().Get("Cache-Control"); got != "private, max-age=3600" {
		t.Errorf("handler Cache-Control overwritten: %q", got)
	}
}

func TestCORS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CORS(DefaultCORS([]string{"https://sewain.hallojanu.me", "http://localhost:3000/"}))(next)

	do := func(method, origin string, preflight bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/state", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if preflight {
			req.Header.Set("Access-Control-Request-Method", "POST")
			req.Header.Set("Access-Control-Request-Headers", "content-type")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := do(http.MethodGet, "https://sewain.hallojanu.me", false)
	if rr.Code != 200 || rr.Header().Get("Access-Control-Allow-Origin") != "https://sewain.hallojanu.me" ||
		rr.Header().Get("Access-Control-Allow-Credentials") != "true" ||
		!strings.Contains(rr.Header().Get("Access-Control-Expose-Headers"), RequestIDHeader) {
		t.Errorf("allowed origin: %d %v", rr.Code, rr.Header())
	}
	if rr := do(http.MethodGet, "http://localhost:3000", false); rr.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Errorf("trailing slash in config not normalized: %v", rr.Header())
	}

	rr = do(http.MethodGet, "https://evil.example", false)
	if rr.Header().Get("Access-Control-Allow-Origin") != "" || rr.Header().Get("Vary") != "Origin" {
		t.Errorf("disallowed origin got CORS headers: %v", rr.Header())
	}

	rr = do(http.MethodOptions, "https://sewain.hallojanu.me", true)
	if rr.Code != http.StatusNoContent || rr.Header().Get("Access-Control-Max-Age") != "600" ||
		!strings.Contains(rr.Header().Get("Access-Control-Allow-Methods"), "POST") ||
		!strings.Contains(rr.Header().Get("Access-Control-Allow-Headers"), "Idempotency-Key") {
		t.Errorf("preflight: %d %v", rr.Code, rr.Header())
	}

	rr = do(http.MethodOptions, "https://evil.example", true)
	if rr.Code != http.StatusForbidden || rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("disallowed preflight: %d %v", rr.Code, rr.Header())
	}

	if rr := do(http.MethodGet, "", false); rr.Code != 200 || rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("same-origin request: %d %v", rr.Code, rr.Header())
	}
}

func TestCORSWildcardNeverWithCredentials(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	o := DefaultCORS([]string{"*"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://any.example")

	rr := httptest.NewRecorder()
	CORS(o)(next).ServeHTTP(rr, req)
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("wildcard honored together with credentials")
	}
	o.AllowCredentials = false
	rr = httptest.NewRecorder()
	CORS(o)(next).ServeHTTP(rr, req)
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("wildcard without credentials should allow any origin")
	}
}

func TestClientIP(t *testing.T) {
	res, err := NewIPResolver(ParseCIDRList("172.16.0.0/12, 127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		remote, cf, xff, want string
	}{
		{"198.51.100.7:1234", "1.2.3.4", "5.6.7.8", "198.51.100.7"},                  // untrusted peer: headers ignored
		{"172.18.0.5:1234", "203.0.113.9", "5.6.7.8", "203.0.113.9"},                 // via Caddy + Cloudflare
		{"172.18.0.5:1234", "", "9.9.9.9, 203.0.113.10, 172.18.0.2", "203.0.113.10"}, // rightmost untrusted hop
		{"127.0.0.1:1234", "garbage", "", "127.0.0.1"},
		{"[::ffff:172.18.0.5]:1", "", "203.0.113.11", "203.0.113.11"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = c.remote
		if c.cf != "" {
			req.Header.Set("CF-Connecting-IP", c.cf)
		}
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := res.ClientIP(req); got != c.want {
			t.Errorf("%+v: got %s", c, got)
		}
	}
	if _, err := NewIPResolver([]string{"not-a-cidr"}); err == nil {
		t.Error("invalid CIDR accepted")
	}
}

func TestRecorderUnwrapsForHijack(t *testing.T) {
	var w http.ResponseWriter = newRecorder(httptest.NewRecorder())
	if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
		t.Fatal("recorder must expose Unwrap for websocket hijacking")
	}
}

func TestParseLevel(t *testing.T) {
	if ParseLevel("DEBUG") != slog.LevelDebug || ParseLevel("warn") != slog.LevelWarn || ParseLevel("") != slog.LevelInfo || ParseLevel("nope") != slog.LevelInfo {
		t.Fatal("ParseLevel")
	}
}
