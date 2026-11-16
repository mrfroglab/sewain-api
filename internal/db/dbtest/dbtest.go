// Package dbtest gives integration tests a fresh, migrated database each. Tests are skipped when TEST_DATABASE_URL
// is empty, so `go test ./...` without Postgres stays green. Start one with: export TEST_DATABASE_URL=$(scripts/test-db.sh up)
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mrfroglab/sewain-api/internal/db"
)

var (
	tplOnce sync.Once
	tplName string
	tplErr  error
)

// URL returns TEST_DATABASE_URL or skips the test.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set (export TEST_DATABASE_URL=$(scripts/test-db.sh up))")
	}
	return u
}

func withDB(raw, name string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Path = "/" + name
	return u.String()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func admin(ctx context.Context, raw string) (*pgx.Conn, error) { return pgx.Connect(ctx, raw) }

// template migrates one template database per test binary.
func template(ctx context.Context, raw string) (string, error) {
	tplOnce.Do(func() {
		tplName = "sewain_tpl_" + randHex(4)
		conn, err := admin(ctx, raw)
		if err != nil {
			tplErr = err
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+tplName); err != nil {
			tplErr = err
			return
		}
		pool, err := db.Open(ctx, withDB(raw, tplName))
		if err != nil {
			tplErr = err
			return
		}
		_, tplErr = db.Migrate(ctx, pool)
		pool.Close()
	})
	return tplName, tplErr
}

// NewDB creates an empty database (no migrations) and drops it after the test. Returns its URL.
func NewDB(t testing.TB) string {
	t.Helper()
	raw := URL(t)
	ctx := context.Background()
	name := "sewain_t_" + randHex(6)
	conn, err := admin(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { drop(raw, name) })
	return withDB(raw, name)
}

// NewPool returns a pool on a fresh copy of the migrated schema.
func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	raw := URL(t)
	ctx := context.Background()
	tpl, err := template(ctx, raw)
	if err != nil {
		t.Fatalf("template database: %v", err)
	}
	name := "sewain_t_" + randHex(6)
	conn, err := admin(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, tpl))
	conn.Close(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, withDB(raw, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		drop(raw, name)
	})
	return pool
}

func drop(raw, name string) {
	ctx := context.Background()
	if conn, err := admin(ctx, raw); err == nil {
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		conn.Close(ctx)
	}
}
