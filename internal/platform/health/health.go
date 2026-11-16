// Package health serves liveness and readiness endpoints and carries the build version.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Build information, injected at link time (see Dockerfile):
//
//	-ldflags "-X github.com/mrfroglab/sewain-api/internal/platform/health.Version=v2.0.0
//	          -X github.com/mrfroglab/sewain-api/internal/platform/health.Commit=abc1234"
var (
	Version = "dev"
	Commit  = "unknown"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Live is GET /api/health: the process is up and serving. It never touches the database, so a slow or
// down database does not make Docker restart a healthy process (that is what /api/ready is for).
func Live() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version, "commit": Commit})
	})
}

// Readiness configures GET /api/ready. The functions are injected so this package does not depend on the
// database or migration packages.
type Readiness struct {
	// Ping checks the database connection (e.g. pool.Ping).
	Ping func(ctx context.Context) error
	// MigrationVersion returns the applied schema version (e.g. goose.GetDBVersion). Optional.
	MigrationVersion func(ctx context.Context) (int64, error)
	// MinMigration is the schema version this binary needs; a lower applied version means not ready.
	// Zero skips the comparison.
	MinMigration int64
	// Timeout bounds all checks together. Default 2 s.
	Timeout time.Duration
}

// NotReadyMessage is the error text when a dependency is not available.
const NotReadyMessage = "Server belum siap."

// Handler returns 200 {"ok":true,...} when every check passes and 503 {"ok":false,"error":...} otherwise.
// Check details say only which check failed, not the underlying error, since the endpoint is public.
func (rd Readiness) Handler() http.Handler {
	timeout := rd.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		checks := map[string]string{}
		ok := true
		fail := func(name, state string) { checks[name] = state; ok = false }

		if rd.Ping == nil {
			fail("db", "not_configured")
		} else if err := rd.Ping(ctx); err != nil {
			fail("db", "down")
		} else {
			checks["db"] = "ok"
		}

		var version int64
		if rd.MigrationVersion != nil && checks["db"] == "ok" {
			v, err := rd.MigrationVersion(ctx)
			switch {
			case err != nil:
				fail("migrations", "unknown")
			case rd.MinMigration > 0 && v < rd.MinMigration:
				fail("migrations", "behind")
			default:
				checks["migrations"] = "ok"
			}
			version = v
		}

		body := map[string]any{"ok": ok, "checks": checks, "version": Version, "commit": Commit}
		if rd.MigrationVersion != nil {
			body["migration"] = version
		}
		status := http.StatusOK
		if !ok {
			status = http.StatusServiceUnavailable
			body["error"] = NotReadyMessage
		}
		writeJSON(w, status, body)
	})
}
