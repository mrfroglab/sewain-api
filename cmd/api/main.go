// Command api runs the Sewain backend (PRD §13): Go + Postgres/PostGIS, transactional services, WebSocket change
// feed, photo evidence.
//
//	api                  serve (migrations run at start under an advisory lock)
//	api migrate [up]     apply pending migrations and exit
//	api migrate status   list migrations
//	api healthcheck      Docker HEALTHCHECK (distroless has no shell or curl)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/httpapi"
	"github.com/mrfroglab/sewain-api/internal/service"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		c := http.Client{Timeout: 3 * time.Second}
		res, err := c.Get("http://127.0.0.1:" + port + "/api/health")
		if err != nil || res.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var err error
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		err = migrate(os.Args[2:])
	} else {
		err = run(log)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func migrate(args []string) error {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL is required")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	cmd := "up"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "up":
		n, err := db.Migrate(ctx, pool)
		if err != nil {
			return err
		}
		fmt.Printf("applied %d migration(s)\n", n)
		return nil
	case "status":
		return db.MigrationStatus(ctx, pool, os.Stdout)
	}
	return fmt.Errorf("unknown migrate command %q (use up | status)", cmd)
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.OpenWithRetry(ctx, cfg.DatabaseURL, 30, 2*time.Second, func(attempt int) {
		log.Info("waiting for database", "attempt", attempt)
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	if cfg.MigrateOnStart {
		n, err := db.Migrate(ctx, pool)
		if err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		log.Info("migrations applied", "count", n)
	}
	svc := service.New(pool, service.Options{DemoMode: cfg.DemoMode, SnapshotScope: cfg.SnapshotScope, Log: log})
	if seeded, err := svc.SeedIfEmpty(ctx); err != nil {
		return fmt.Errorf("seed: %w", err)
	} else if seeded {
		log.Info("empty database: demo data seeded")
	}
	srv := httpapi.New(cfg, svc, log)
	go svc.RunScheduler(ctx, 15*time.Second)

	httpSrv := &http.Server{Addr: ":" + cfg.Port, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
	}()
	log.Info("sewain-api listening", "port", cfg.Port, "env", cfg.AppEnv, "demoMode", cfg.DemoMode, "snapshotScope", svc.SnapshotScope())
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
