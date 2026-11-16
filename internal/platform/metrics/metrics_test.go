package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != 200 {
		t.Fatalf("scrape status %d", rr.Code)
	}
	return rr.Body.String()
}

func TestInstrumentUsesRoutePattern(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/photos/{id}", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "x") })
	mux.HandleFunc("POST /api/v1/actions/{name}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusConflict) })
	h := middleware.Chain(m.Instrument(mux), middleware.RequestID)

	for _, p := range []string{"/api/v1/photos/p-1", "/api/v1/photos/p-2", "/api/v1/photos/p-3"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/actions/createBooking", nil))
	for i := range 5 { // scanner noise must not create new series
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/wp-admin/"+strings.Repeat("x", i), nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PROPFIND", "/api/v1/photos/p-1", nil))

	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "GET /api/v1/photos/{id}", "200")); got != 3 {
		t.Errorf("photo requests = %v", got)
	}
	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("POST", "POST /api/v1/actions/{name}", "409")); got != 1 {
		t.Errorf("action requests = %v", got)
	}
	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", UnmatchedRoute, "404")); got != 5 {
		t.Errorf("unmatched = %v", got)
	}
	// Labels are bounded: 2 routes + unmatched GET + one OTHER-method series.
	if n := testutil.CollectAndCount(m.httpRequests); n != 4 {
		t.Errorf("request series = %d, want 4", n)
	}
	if n := testutil.CollectAndCount(m.httpDuration); n != 4 {
		t.Errorf("duration series = %d, want 4", n)
	}
	body := scrape(t, m)
	for _, s := range []string{"p-1", "createBooking", "wp-admin", "PROPFIND"} {
		if strings.Contains(body, s) {
			t.Errorf("raw value %q leaked into labels", s)
		}
	}
}

func TestInstrumentRecordsRouteForLogger(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/state", func(http.ResponseWriter, *http.Request) {})
	var route string
	outer := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			route = middleware.RouteFrom(r.Context())
		})
	}
	// Timeout swaps the request (WithContext) between the logger and the mux; the route must survive that.
	h := middleware.Chain(m.Instrument(mux), middleware.RequestID, outer, middleware.Timeout(time.Second, middleware.TimeoutOptions{}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/state", nil))
	if route != "GET /api/v1/state" {
		t.Fatalf("route = %q", route)
	}
}

func TestAllMetricsRegistered(t *testing.T) {
	m := New()
	m.WebSocketOpened()
	m.WebSocketOpened()
	m.WebSocketClosed()
	m.ObserveJob("ticker", 20*time.Millisecond, nil)
	_ = m.TimeJob("retention", func() error { return errors.New("x") })
	m.SetOutboxPending("push", 7)
	m.RateLimited("auth")
	m.httpRequests.WithLabelValues("GET", "r", "200").Inc()
	m.httpDuration.WithLabelValues("GET", "r").Observe(0.1)

	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := m.RegisterPool(pool); err != nil {
		t.Fatal(err)
	}

	body := scrape(t, m)
	for _, name := range []string{
		"sewain_http_request_duration_seconds_bucket",
		"sewain_http_requests_total",
		"sewain_http_requests_in_flight",
		"sewain_websocket_connections_active 1",
		`sewain_job_duration_seconds_count{job="ticker",result="ok"} 1`,
		`sewain_job_duration_seconds_count{job="retention",result="error"} 1`,
		`sewain_outbox_pending{channel="push"} 7`,
		`sewain_http_rate_limited_total{profile="auth"} 1`,
		"sewain_db_pool_max_conns",
		"sewain_db_pool_acquire_total 0",
		"go_goroutines",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q missing", name)
		}
	}
	problems, err := testutil.GatherAndLint(m.Registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p.Metric, "sewain_") {
			t.Errorf("lint %s: %s", p.Metric, p.Text)
		}
	}
}

func TestMetricsServer(t *testing.T) {
	srv := NewServer("127.0.0.1:0", New())
	rr := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "go_goroutines") {
		t.Fatalf("metrics server: %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/state", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("metrics server serves other paths: %d", rr.Code)
	}
}

func TestAddrFromEnv(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")
	if AddrFromEnv() != DefaultAddr {
		t.Error("default")
	}
	t.Setenv("METRICS_ADDR", ":9100")
	if AddrFromEnv() != ":9100" {
		t.Error("override")
	}
	t.Setenv("METRICS_ADDR", "off")
	if AddrFromEnv() != "" {
		t.Error("off")
	}
}

func TestInstrumentCountsPanicAs500(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("x") })
	func() {
		defer func() { _ = recover() }()
		m.Instrument(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	}()
	if got := testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "GET /boom", "500")); got != 1 {
		t.Fatalf("panic counted %v times as 500", got)
	}
}
