package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const migrationsDir = "../db/migrations"

var migrationName = regexp.MustCompile(`^(\d{5})_.*\.sql$`)

// notifyMigrations returns the migrations in the notify range (00140–00159) only.
func notifyMigrations(t *testing.T) fs.FS {
	t.Helper()
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if n, _ := strconv.Atoi(m[1]); n < 140 || n > 159 {
			continue
		}
		b, err := os.ReadFile(migrationsDir + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: b}
	}
	if len(out) == 0 {
		t.Fatal("no notify migrations found")
	}
	return out
}

// testPool gives each test its own schema with a minimal users table and the notify migrations applied.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "notify_test_" + hex.EncodeToString(b)

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	// Fixture: the core owns users; this is the minimum the notify tables reference (+ email for the email channel).
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS users (id text PRIMARY KEY, role text DEFAULT 'user', email text)`); err != nil {
		t.Fatal(err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, notifyMigrations(t), goose.WithAllowOutofOrder(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	// Down works too (checked once per test schema, then re-applied).
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

func addUser(t *testing.T, db DB, id, email string) {
	t.Helper()
	var e any
	if email != "" {
		e = email
	}
	if _, err := db.Exec(context.Background(), `INSERT INTO users (id, email) VALUES ($1, $2)`, id, e); err != nil {
		t.Fatal(err)
	}
}
