package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const migrationsDir = "../db/migrations"

var migrationName = regexp.MustCompile(`^(\d{5})_.*\.sql$`)

// paymentMigrations returns only the payments range (00200–00249).
func paymentMigrations(t *testing.T) fs.FS {
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
		if n, _ := strconv.Atoi(m[1]); n < 200 || n > 249 {
			continue
		}
		b, err := os.ReadFile(migrationsDir + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: b}
	}
	if len(out) == 0 {
		t.Fatal("no payments migrations")
	}
	return out
}

// fixtureSQL is the minimum of the core schema the payments package reads (users, bookings).
const fixtureSQL = `
CREATE TABLE users (id text PRIMARY KEY);
CREATE TABLE bookings (
  id text PRIMARY KEY,
  renter_id text NOT NULL REFERENCES users (id),
  provider_id text NOT NULL REFERENCES users (id),
  status text NOT NULL,
  rent_total bigint NOT NULL,
  commission_amount bigint NOT NULL DEFAULT 0,
  payment jsonb NOT NULL,
  agreement jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);`

// testPool gives each test its own schema with the fixture and the payments migrations (checked down/up).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set (export TEST_DATABASE_URL=$(scripts/test-db.sh up))")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "payments_test_" + hex.EncodeToString(b)
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
	cfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, fixtureSQL); err != nil {
		t.Fatal(err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, paymentMigrations(t), goose.WithAllowOutofOrder(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

// env is a service on a fresh schema with a fake provider and a controllable clock.
type env struct {
	t    *testing.T
	ctx  context.Context
	db   *pgxpool.Pool
	s    *Service
	fake *FakeProvider
	now  time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), db: testPool(t), fake: NewFakeProvider("fake-token"),
		now: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)}
	s, err := NewService(e.db, SQLBookings{}, Config{Provider: "fake", IntentTTL: 30 * time.Minute, MDRBasisPoints: 70, CommissionActive: true}, e.fake)
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return e.now }
	e.s = s
	for _, u := range []string{"u-renter", "u-provider", "u-other"} {
		e.exec(`INSERT INTO users (id) VALUES ($1)`, u)
	}
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// booking inserts an ACCEPTED booking: rent 100.000, deposit as given, commission 10% split 50:50.
func (e *env) booking(id string, deposit int64) {
	e.t.Helper()
	e.exec(`INSERT INTO bookings (id, renter_id, provider_id, status, rent_total, commission_amount, payment, agreement)
		VALUES ($1, 'u-renter', 'u-provider', 'ACCEPTED', 100000, 10000, jsonb_build_object('deposit', $2::bigint),
		'{"lateFeeCap": 300000, "commission": {"active": true, "mode": "persen", "rate": 10, "bearer": "split",
		  "splitRenterPercent": 50, "amount": 10000, "charged": 10000, "renterPays": 5000, "providerPays": 5000}}')`, id, deposit)
}

func (e *env) webhook(eventID, typ, ref, reference string, amount int64) WebhookResult {
	e.t.Helper()
	r := e.fake.Webhook(eventID, typ, ref, reference, amount)
	body, _ := io.ReadAll(r.Body)
	res, err := e.s.HandleWebhook(e.ctx, "fake", r, body)
	if err != nil {
		e.t.Fatalf("webhook %s: %v", eventID, err)
	}
	return res
}

func (e *env) pay(in Intent, eventID string) WebhookResult {
	e.t.Helper()
	return e.webhook(eventID, EventPaymentSucceeded, *in.ProviderRef, in.ID, in.Amount)
}

func (e *env) balance(account string) int64 {
	e.t.Helper()
	b, err := Balance(e.ctx, e.db, account)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func (e *env) intent(id string) Intent {
	e.t.Helper()
	in, err := getIntent(e.ctx, e.db, `id = $1`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return in
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// assertBalanced checks the global ledger invariant.
func (e *env) assertBalanced() {
	e.t.Helper()
	all, err := AllBalances(e.ctx, e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	var total int64
	for _, v := range all {
		total += v
	}
	if total != 0 {
		e.t.Fatalf("ledger total %d: %v", total, all)
	}
}
