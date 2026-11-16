package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

type poolStater interface{ Stat() *pgxpool.Stat }

// PoolCollector exports pgxpool statistics at scrape time.
type PoolCollector struct {
	pool poolStater

	acquired, idle, total, constructing, max *prometheus.Desc
	acquires, canceled, empty, newConns      *prometheus.Desc
	lifetimeDestroy, idleDestroy, acquireDur *prometheus.Desc
}

// NewPoolCollector returns a collector for pool. Register it with RegisterPool or Registry.MustRegister.
func NewPoolCollector(pool *pgxpool.Pool) *PoolCollector { return newPoolCollector(pool) }

func newPoolCollector(p poolStater) *PoolCollector {
	d := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "db_pool", name), help, nil, nil)
	}
	return &PoolCollector{
		pool:            p,
		acquired:        d("acquired_conns", "Connections currently checked out."),
		idle:            d("idle_conns", "Idle connections in the pool."),
		total:           d("total_conns", "Total connections in the pool."),
		constructing:    d("constructing_conns", "Connections being established."),
		max:             d("max_conns", "Configured maximum pool size."),
		acquires:        d("acquire_total", "Successful connection acquisitions."),
		canceled:        d("canceled_acquire_total", "Acquisitions cancelled by their context."),
		empty:           d("empty_acquire_total", "Acquisitions that had to wait because the pool was empty."),
		newConns:        d("new_conns_total", "Connections opened."),
		lifetimeDestroy: d("max_lifetime_destroy_total", "Connections closed for exceeding MaxConnLifetime."),
		idleDestroy:     d("max_idle_destroy_total", "Connections closed for exceeding MaxConnIdleTime."),
		acquireDur:      d("acquire_duration_seconds_total", "Total time spent acquiring connections."),
	}
}

// Describe implements prometheus.Collector.
func (c *PoolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquired, c.idle, c.total, c.constructing, c.max, c.acquires,
		c.canceled, c.empty, c.newConns, c.lifetimeDestroy, c.idleDestroy, c.acquireDur} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *PoolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	g := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }
	k := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	g(c.acquired, float64(s.AcquiredConns()))
	g(c.idle, float64(s.IdleConns()))
	g(c.total, float64(s.TotalConns()))
	g(c.constructing, float64(s.ConstructingConns()))
	g(c.max, float64(s.MaxConns()))
	k(c.acquires, float64(s.AcquireCount()))
	k(c.canceled, float64(s.CanceledAcquireCount()))
	k(c.empty, float64(s.EmptyAcquireCount()))
	k(c.newConns, float64(s.NewConnsCount()))
	k(c.lifetimeDestroy, float64(s.MaxLifetimeDestroyCount()))
	k(c.idleDestroy, float64(s.MaxIdleDestroyCount()))
	k(c.acquireDur, s.AcquireDuration().Seconds())
}

// RegisterPool adds pgxpool statistics for pool to m.
func (m *Metrics) RegisterPool(pool *pgxpool.Pool) error {
	return m.Registry.Register(NewPoolCollector(pool))
}
