package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Migrations holds every module's goose migrations. Number ranges keep parallel work apart: core 00001–00099,
// auth 00100–00119, media 00120–00139, notify 00140–00159, ops 00160–00179, payments 00200–00249.
//
//go:embed migrations/*.sql
var Migrations embed.FS

func provider(pool *pgxpool.Pool) (*goose.Provider, *sql.DB, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	sub, err := fs.Sub(Migrations, "migrations")
	if err != nil {
		return nil, nil, err
	}
	// A session-level advisory lock serialises concurrent instances starting at the same time.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(LockMigrate))
	if err != nil {
		return nil, nil, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub,
		goose.WithAllowOutofOrder(true), // modules add migrations in their own ranges at different times
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		_ = sqlDB.Close()
		return nil, nil, err
	}
	return p, sqlDB, nil
}

// Migrate applies all pending migrations (under the advisory lock) and returns how many ran.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	p, sqlDB, err := provider(pool)
	if err != nil {
		return 0, err
	}
	defer sqlDB.Close()
	res, err := p.Up(ctx)
	return len(res), err
}

// MigrationStatus writes one line per migration (applied or pending).
func MigrationStatus(ctx context.Context, pool *pgxpool.Pool, w io.Writer) error {
	p, sqlDB, err := provider(pool)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	list, err := p.Status(ctx)
	if err != nil {
		return err
	}
	for _, s := range list {
		applied := "pending"
		if s.State == goose.StateApplied {
			applied = "applied " + s.AppliedAt.UTC().Format("2006-01-02 15:04:05Z")
		}
		fmt.Fprintf(w, "%05d  %-40s %s\n", s.Source.Version, s.Source.Path, applied)
	}
	return nil
}
