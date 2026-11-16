// Package metrics exposes the API's Prometheus metrics (ROADMAP F5.3) on a separate, internal-only server.
package metrics

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
)

const namespace = "sewain"

// UnmatchedRoute labels requests that no ServeMux pattern matched (404/405 noise, scanners). Using the raw
// path instead would let any client create unbounded label values.
const UnmatchedRoute = "unmatched"

// Metrics owns a private registry, so tests and multiple instances in one process never collide with the
// global default registry.
type Metrics struct {
	Registry *prometheus.Registry

	httpDuration  *prometheus.HistogramVec
	httpRequests  *prometheus.CounterVec
	httpInFlight  prometheus.Gauge
	wsActive      prometheus.Gauge
	jobDuration   *prometheus.HistogramVec
	outboxPending *prometheus.GaugeVec
	rateLimited   *prometheus.CounterVec
}

// New creates the metrics and registers them together with the Go runtime and process collectors.
func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help:    "HTTP request latency by ServeMux route pattern.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests by route pattern and status code.",
		}, []string{"method", "route", "status"}),
		httpInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "http", Name: "requests_in_flight",
			Help: "HTTP requests currently being served (excluding open WebSockets).",
		}),
		wsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "websocket", Name: "connections_active",
			Help: "Open WebSocket connections on this instance.",
		}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "job", Name: "duration_seconds",
			Help:    "Duration of periodic jobs (ticker, retention, outbox dispatch) by outcome.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"job", "result"}),
		outboxPending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "outbox", Name: "pending",
			Help: "Notification outbox rows waiting to be sent, by channel.",
		}, []string{"channel"}),
		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "http", Name: "rate_limited_total",
			Help: "Requests rejected with 429 by rate-limit profile.",
		}, []string{"profile"}),
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpDuration, m.httpRequests, m.httpInFlight, m.wsActive, m.jobDuration, m.outboxPending, m.rateLimited,
	)
	return m
}

var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

func methodLabel(m string) string {
	if knownMethods[m] {
		return m
	}
	return "OTHER"
}

// Instrument records duration and status per route. It must wrap the *http.ServeMux directly (be the
// innermost middleware): ServeMux sets r.Pattern on the request it receives, and Instrument reads it after
// the handler returns. It also records the pattern with middleware.SetRoute so the outer access logger can
// print it. WebSocket upgrades are counted but kept out of the latency histogram, since their "duration" is
// the connection lifetime.
func (m *Metrics) Instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := middleware.IsWebSocket(r)
		if !ws {
			m.httpInFlight.Inc()
			defer m.httpInFlight.Dec()
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		completed := false
		defer func() { // runs on panic too; no recover, so Recover keeps the original stack

			route := r.Pattern
			if route == "" {
				route = UnmatchedRoute
			} else {
				middleware.SetRoute(r.Context(), route)
			}
			method := methodLabel(r.Method)
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			if !completed {
				status = http.StatusInternalServerError
			}
			m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
			if !ws {
				m.httpDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
			}
		}()
		next.ServeHTTP(sw, r)
		completed = true
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// WebSocketOpened / WebSocketClosed track open WebSocket connections. Call them in pairs (defer the close).
func (m *Metrics) WebSocketOpened() { m.wsActive.Inc() }

// WebSocketClosed is the counterpart of WebSocketOpened.
func (m *Metrics) WebSocketClosed() { m.wsActive.Dec() }

// ObserveJob records one run of a periodic job. Typical use:
//
//	start := time.Now(); err := sweep(ctx); m.ObserveJob("retention", time.Since(start), err)
func (m *Metrics) ObserveJob(job string, d time.Duration, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.jobDuration.WithLabelValues(job, result).Observe(d.Seconds())
}

// TimeJob runs fn and records its duration under job.
func (m *Metrics) TimeJob(job string, fn func() error) error {
	start := time.Now()
	err := fn()
	m.ObserveJob(job, time.Since(start), err)
	return err
}

// SetOutboxPending sets the number of unsent outbox rows for a channel (inapp, push, email, wa).
func (m *Metrics) SetOutboxPending(channel string, n int) {
	m.outboxPending.WithLabelValues(channel).Set(float64(n))
}

// RateLimited counts a 429; pass it as ratelimit.Options.OnLimited (adapting the signature).
func (m *Metrics) RateLimited(profile string) { m.rateLimited.WithLabelValues(profile).Inc() }

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}

// DefaultAddr keeps the metrics port off the public interface. Inside Docker, set METRICS_ADDR=:9090 and do
// not publish the port: Prometheus scrapes it over the internal Docker network, never through Caddy.
const DefaultAddr = "127.0.0.1:9090"

// AddrFromEnv returns METRICS_ADDR, DefaultAddr when unset, or "" when set to "off" (metrics disabled).
func AddrFromEnv() string {
	v := strings.TrimSpace(os.Getenv("METRICS_ADDR"))
	switch strings.ToLower(v) {
	case "":
		return DefaultAddr
	case "off", "false", "0", "disabled":
		return ""
	}
	return v
}

// NewServer returns the internal metrics server: GET /metrics only.
func NewServer(addr string, m *Metrics) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
}
