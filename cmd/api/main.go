// Command api runs the Sewain backend (PRD §13): Go + Postgres/PostGIS, WebSocket change feed, photo evidence.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/httpapi"
	"github.com/mrfroglab/sewain-api/internal/store"
)

func main() {
	// "api healthcheck" is used by the Docker HEALTHCHECK (distroless has no shell or curl).
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
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var db *store.Store
	for attempt := 1; ; attempt++ { // wait for Postgres on cold start
		db, err = store.Open(ctx, cfg.DatabaseURL)
		if err == nil || attempt >= 30 {
			break
		}
		log.Info("waiting for database", "attempt", attempt)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	data, err := db.Load(ctx)
	if err != nil {
		return err
	}
	eng := domain.NewEngine(&domain.Data{})
	srv := httpapi.New(cfg, eng, db, log)
	if data == nil {
		log.Info("empty database: seeding demo data")
		if err := srv.Seed(ctx); err != nil {
			return err
		}
	} else {
		eng.Replace(data)
	}
	go srv.RunTicker(ctx, 15*time.Second)

	httpSrv := &http.Server{Addr: ":" + cfg.Port, Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sctx)
	}()
	log.Info("sewain-api listening", "port", cfg.Port, "env", cfg.AppEnv, "demoMode", cfg.DemoMode)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
