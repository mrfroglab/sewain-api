package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrfroglab/sewain-api/internal/platform/health"
)

// TestPoolAndReadinessWithDatabase runs against a real Postgres (scripts/test-db.sh, CI service container).
func TestPoolAndReadinessWithDatabase(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var one int
	if err := pool.QueryRow(ctx, "select 1").Scan(&one); err != nil {
		t.Fatal(err)
	}

	m := New()
	if err := m.RegisterPool(pool); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, m)
	for _, want := range []string{"sewain_db_pool_acquire_total 1", "sewain_db_pool_total_conns 1", "sewain_db_pool_new_conns_total 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in scrape", want)
		}
	}

	ready := health.Readiness{
		Ping: pool.Ping,
		MigrationVersion: func(ctx context.Context) (int64, error) {
			var v int64
			err := pool.QueryRow(ctx, "select 7").Scan(&v)
			return v, err
		},
		MinMigration: 7,
	}
	rr := httptest.NewRecorder()
	ready.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ready", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("ready with live DB: %d %s", rr.Code, rr.Body)
	}

	pool.Close()
	rr = httptest.NewRecorder()
	ready.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ready", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready with closed pool: %d %s", rr.Code, rr.Body)
	}
}
