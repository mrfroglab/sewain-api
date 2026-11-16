// Command api runs the Sewain backend (PRD §13): Go + Postgres/PostGIS, transactional services, OTP sessions, KYC,
// photo evidence, per-user WebSocket change feed and the notification outbox.
//
//	api                          serve (migrations run at start under an advisory lock)
//	api migrate [up|status]      apply pending migrations and exit / list migrations
//	api admin grant <phone>      give a user the admin role (the user must have signed in once)
//	api admin revoke <phone>     take the admin role away
//	api vapid gen                print a new VAPID key pair for web push
//	api healthcheck              Docker HEALTHCHECK (distroless has no shell or curl)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/mrfroglab/sewain-api/internal/app"
	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/notify"
	"github.com/mrfroglab/sewain-api/internal/platform/metrics"
	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
	"github.com/mrfroglab/sewain-api/internal/platform/server"
	"github.com/mrfroglab/sewain-api/internal/service"
)

func main() {
	log := middleware.NewLogger(os.Stdout, middleware.ParseLevel(os.Getenv("LOG_LEVEL")))
	if err := cli(os.Args[1:], os.Stdout, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func cli(args []string, out io.Writer, log *slog.Logger) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "":
		return serve(log)
	case "healthcheck":
		return healthcheck()
	case "migrate":
		return migrate(args[1:], out)
	case "admin":
		return admin(args[1:], out)
	case "vapid":
		if len(args) < 2 || args[1] != "gen" {
			return errors.New("usage: api vapid gen")
		}
		pub, priv, err := notify.GenerateVAPIDKeys()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n", pub, priv)
		return err
	}
	return fmt.Errorf("unknown command %q (serve | migrate | admin | vapid | healthcheck)", cmd)
}

func healthcheck() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	c := http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://127.0.0.1:" + port + "/api/health")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", res.Status)
	}
	return nil
}

func openDB(ctx context.Context) (*service.Service, func(), error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, nil, errors.New("DATABASE_URL is required")
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	return service.New(pool, service.Options{}), pool.Close, nil
}

func migrate(args []string, out io.Writer) error {
	ctx := context.Background()
	svc, closeDB, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer closeDB()
	cmd := "up"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "up":
		n, err := db.Migrate(ctx, svc.Pool())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "applied %d migration(s)\n", n)
		return err
	case "status":
		return db.MigrationStatus(ctx, svc.Pool(), out)
	}
	return fmt.Errorf("unknown migrate command %q (use up | status)", cmd)
}

// admin grant|revoke <phone> (F2.5). Audited as actor "cli".
func admin(args []string, out io.Writer) error {
	if len(args) != 2 || (args[0] != "grant" && args[0] != "revoke") {
		return errors.New("usage: api admin grant|revoke <phone>")
	}
	ctx := context.Background()
	svc, closeDB, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer closeDB()
	role := "admin"
	if args[0] == "revoke" {
		role = "user"
	}
	id, err := svc.SetRoleByPhone(ctx, args[1], role, "cli")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s: role=%s\n", id, role)
	return err
}

// serve runs the API until SIGINT/SIGTERM, then shuts down in order: WebSockets are told to go away, in-flight
// requests finish (≤ 8 s), background runners stop (the dispatcher records sends in progress), the pool closes.
func serve(log *slog.Logger) error {
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
	m := metrics.New()
	if err := m.RegisterPool(pool); err != nil {
		return err
	}
	a, err := app.New(cfg, pool, log, app.Options{Metrics: m})
	if err != nil {
		return err
	}
	if seeded, err := a.Svc.SeedIfEmpty(ctx); err != nil {
		return fmt.Errorf("seed: %w", err)
	} else if seeded {
		log.Info("empty database: demo data seeded")
	}

	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	var runners sync.WaitGroup
	runners.Add(1)
	go func() {
		defer runners.Done()
		a.Run(runCtx)
	}()
	defer func() {
		cancelRun()
		runners.Wait()
	}()

	api := &http.Server{Addr: ":" + cfg.Port, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	api.RegisterOnShutdown(a.Close) // hijacked WebSockets are not tracked by Shutdown
	log.Info("sewain-api starting", "port", cfg.Port, "env", cfg.AppEnv, "demoMode", cfg.DemoMode, "snapshotScope", a.Svc.SnapshotScope(),
		"paymentMode", a.Svc.PaymentMode(), "otpProvider", cfg.Auth.OTPProvider, "metrics", cfg.MetricsAddr)

	srvCtx, cancelSrv := context.WithCancel(ctx)
	defer cancelSrv()
	errc := make(chan error, 2)
	servers := 1
	go func() { errc <- server.Run(srvCtx, api, server.Options{Name: "api", Log: log}) }()
	if cfg.MetricsAddr != "" {
		servers++
		go func() { errc <- server.Run(srvCtx, metrics.NewServer(cfg.MetricsAddr, m), server.Options{Name: "metrics", Log: log}) }()
	}
	var first error
	for i := 0; i < servers; i++ {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
		cancelSrv() // one server stopping (signal or failure) stops the other
	}
	return first
}
