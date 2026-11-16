// Package authtest provides integration-test fixtures for the auth and kyc packages: a throwaway Postgres
// schema with the auth migrations (00100–00119) applied by goose, a minimal users table, and a
// UserDirectory backed by it. Tests skip when TEST_DATABASE_URL is empty.
package authtest

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mrfroglab/sewain-api/internal/crypto"
	"github.com/pressly/goose/v3"
)

// MigrationRange is the inclusive goose version range this fixture applies (auth + kyc).
const (
	MigrationFirst = 100
	MigrationLast  = 119
)

// UsersFixture is the minimal users table the auth migrations reference. The real table is owned by core.
const UsersFixture = `CREATE TABLE IF NOT EXISTS users (
	id text PRIMARY KEY, phone text UNIQUE, role text DEFAULT 'user', status text DEFAULT 'aktif')`

// DB opens a pool on a fresh schema (dropped on cleanup unless AUTHTEST_KEEP_SCHEMA is set, e.g. to
// inspect it with pg_dump) with the users fixture and the auth migrations.
func DB(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)
	schema := "t_" + randHex(t, 6)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if os.Getenv("AUTHTEST_KEEP_SCHEMA") != "" {
			t.Logf("keeping schema %s (AUTHTEST_KEEP_SCHEMA)", schema)
			return
		}
		c, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = c.Close(context.Background())
		}
	})
	if _, err := pool.Exec(ctx, UsersFixture); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// Migrate applies the migrations in [MigrationFirst, MigrationLast] from internal/db/migrations with goose.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	fsys, err := migrationsFS()
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithAllowOutofOrder(true))
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return err
	}
	// Exercise Down as well, then re-apply, so broken Down sections are caught.
	if _, err := p.DownTo(ctx, 0); err != nil {
		return fmt.Errorf("down: %w", err)
	}
	_, err = p.Up(ctx)
	return err
}

func migrationsFS() (fs.FS, error) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil || n < MigrationFirst || n > MigrationLast {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- test fixture: names listed from the repo's migrations dir
		if err != nil {
			return nil, err
		}
		out[name] = &fstest.MapFile{Data: b}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no migrations in range %d–%d under %s", MigrationFirst, MigrationLast, dir)
	}
	return out, nil
}

// Directory is a UserDirectory over the users fixture table.
type Directory struct {
	DB  *pgxpool.Pool
	seq atomic.Int64
}

func (d *Directory) FindUserByPhone(ctx context.Context, phone string) (string, bool, error) {
	var id string
	err := d.DB.QueryRow(ctx, `SELECT id FROM users WHERE phone = $1`, phone).Scan(&id)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	return id, err == nil, err
}

func (d *Directory) CreateUser(ctx context.Context, tx pgx.Tx, phone string) (string, error) {
	id := fmt.Sprintf("u-%d-%s", d.seq.Add(1), strings.TrimPrefix(phone, "+"))
	_, err := tx.Exec(ctx, `INSERT INTO users (id, phone) VALUES ($1, $2)`, id, phone)
	return id, err
}

func (d *Directory) GetUserAuth(ctx context.Context, userID string) (string, string, error) {
	var role, status string
	err := d.DB.QueryRow(ctx, `SELECT role, status FROM users WHERE id = $1`, userID).Scan(&role, &status)
	return role, status, err
}

// AddUser inserts a user row directly.
func AddUser(t testing.TB, db *pgxpool.Pool, id, phone, role string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `INSERT INTO users (id, phone, role) VALUES ($1, $2, $3)`, id, phone, role); err != nil {
		t.Fatal(err)
	}
}

// Clock is a settable fake clock.
type Clock struct{ t atomic.Int64 }

// NewClock starts at t.
func NewClock(t time.Time) *Clock { c := &Clock{}; c.Set(t); return c }

func (c *Clock) Now() time.Time          { return time.Unix(0, c.t.Load()).UTC() }
func (c *Clock) Set(t time.Time)         { c.t.Store(t.UnixNano()) }
func (c *Clock) Advance(d time.Duration) { c.t.Add(int64(d)) }

func randHex(t testing.TB, n int) string {
	b, err := crypto.RandomBytes(n)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
