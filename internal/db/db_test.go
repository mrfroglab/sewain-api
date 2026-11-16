package db_test

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/db/dbtest"
)

// v1 schema as it was in internal/store/migrations/001_init.sql before v2.
//
//go:embed testdata/v1_001_init.sql
var v1Schema string

func TestMigrateFromEmpty(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, dbtest.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	n, err := db.Migrate(ctx, pool)
	if err != nil || n < 1 {
		t.Fatalf("migrate: %d %v", n, err)
	}
	// Idempotent: nothing left to apply.
	if n, err := db.Migrate(ctx, pool); err != nil || n != 0 {
		t.Fatalf("second run: %d %v", n, err)
	}
	var buf strings.Builder
	if err := db.MigrationStatus(ctx, pool, &buf); err != nil || !strings.Contains(buf.String(), "00001_core_schema.sql") {
		t.Fatalf("status: %v\n%s", err, buf.String())
	}
}

// TestMigrateOverV1Schema: migration 00001 on a database that still has the v1 JSONB tables (and data).
func TestMigrateOverV1Schema(t *testing.T) {
	ctx := context.Background()
	pool, err := db.Open(ctx, dbtest.NewDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, v1Schema); err != nil {
		t.Fatalf("create v1 schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, data) VALUES ('u-old', '{"phone":"0811","role":"user","status":"aktif","joinedAt":1}');
		INSERT INTO sessions (token, user_id) VALUES ('t', 'u-old');
		INSERT INTO settings (key, value) VALUES ('meta', '{}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate over v1: %v", err)
	}
	var hasData, hasV1 bool
	if err := pool.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'data'),
		EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name IN ('verifications', 'wanted', 'settings', 'sessions', 'audit'))`).Scan(&hasData, &hasV1); err != nil {
		t.Fatal(err)
	}
	if hasData || hasV1 {
		t.Fatalf("v1 tables must be gone (users.data=%v, others=%v)", hasData, hasV1)
	}
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("v2 users table: %d %v", users, err)
	}
}

func TestAppendOnlyTriggers(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO photos (id, stage, uploader_id, src, sha256, server_ts, source) VALUES ('p-1', 'checkout', 'u', '/x', '00', now(), 'kamera');
		INSERT INTO audit_log (id, actor_id, action, target, at) VALUES ('a-1', 'u', 'x', 't', now())`); err != nil {
		t.Fatal(err)
	}
	denied := func(sql string) {
		t.Helper()
		_, err := pool.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: want insufficient_privilege, got %v", sql, err)
		}
	}
	denied(`UPDATE photos SET sha256 = 'ff' WHERE id = 'p-1'`)
	denied(`DELETE FROM photos WHERE id = 'p-1'`)
	denied(`TRUNCATE photos`)
	denied(`UPDATE audit_log SET target = 'y'`)
	denied(`DELETE FROM audit_log`)
	// The retention job opts in per transaction.
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL sewain.retention = 'on'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM photos WHERE id = 'p-1'`)
		return err
	})
	if err != nil {
		t.Fatalf("retention delete: %v", err)
	}
	denied(`UPDATE audit_log SET target = 'y'`) // the setting was transaction-local
}

func TestWithTxRetriesSerializationFailures(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	var calls atomic.Int32
	err := db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if calls.Add(1) < 3 {
			return &pgconn.PgError{Code: "40001", Message: "could not serialize access"}
		}
		return nil
	})
	if err != nil || calls.Load() != 3 {
		t.Fatalf("want success on 3rd attempt, got %v after %d", err, calls.Load())
	}
	calls.Store(0)
	err = db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		calls.Add(1)
		return &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
	})
	if !db.Retryable(err) || calls.Load() != db.MaxAttempts {
		t.Fatalf("gives up after %d attempts: %v (%d)", db.MaxAttempts, err, calls.Load())
	}
	calls.Store(0)
	_ = db.WithTx(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		calls.Add(1)
		return errors.New("boom")
	})
	if calls.Load() != 1 {
		t.Fatal("other errors are not retried")
	}
}

func TestAdvisoryLock(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.NewPool(t)
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback(ctx)
	if ok, err := db.TryXactLock(ctx, tx1, db.LockJobs); !ok || err != nil {
		t.Fatalf("first lock: %v %v", ok, err)
	}
	tx2, _ := pool.Begin(ctx)
	defer tx2.Rollback(ctx)
	if ok, _ := db.TryXactLock(ctx, tx2, db.LockJobs); ok {
		t.Fatal("second transaction must not get the lock")
	}
}
