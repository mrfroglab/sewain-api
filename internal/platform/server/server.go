// Package server runs an http.Server until its context ends and then shuts it down gracefully.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// DefaultShutdownTimeout is how long in-flight requests get to finish after SIGTERM. Docker's default stop
// timeout is 10 s, so keep this below it.
const DefaultShutdownTimeout = 8 * time.Second

// Options tunes Run.
type Options struct {
	ShutdownTimeout time.Duration // default DefaultShutdownTimeout
	Log             *slog.Logger  // optional
	Name            string        // for logs, e.g. "api" or "metrics"
	// Listener serves on an existing listener instead of srv.Addr (tests, socket activation).
	Listener net.Listener
}

// Run serves srv until ctx is cancelled, then calls srv.Shutdown and waits up to ShutdownTimeout for
// in-flight requests. Hijacked connections (WebSocket) are not tracked by Shutdown: register a close hook
// with srv.RegisterOnShutdown to tell them to go away. Returns nil on a clean shutdown.
func Run(ctx context.Context, srv *http.Server, o Options) error {
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.Name == "" {
		o.Name = "http"
	}
	errc := make(chan error, 1)
	go func() {
		var err error
		if o.Listener != nil {
			err = srv.Serve(o.Listener)
		} else {
			err = srv.ListenAndServe()
		}
		errc <- err
	}()
	if o.Log != nil {
		o.Log.Info("server listening", "server", o.Name, "addr", srv.Addr)
	}
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	if o.Log != nil {
		o.Log.Info("shutting down", "server", o.Name, "timeout", o.ShutdownTimeout.String())
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
