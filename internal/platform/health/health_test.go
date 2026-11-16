package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, h http.Handler) (int, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ready", nil))
	var m map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Error("health responses must not be cached")
	}
	return rr.Code, m
}

func okPing(context.Context) error { return nil }

func TestLive(t *testing.T) {
	Version, Commit = "v9", "abc"
	defer func() { Version, Commit = "dev", "unknown" }()
	code, m := get(t, Live())
	if code != 200 || m["ok"] != true || m["version"] != "v9" || m["commit"] != "abc" {
		t.Fatalf("%d %v", code, m)
	}
}

func TestReadyOK(t *testing.T) {
	rd := Readiness{Ping: okPing, MigrationVersion: func(context.Context) (int64, error) { return 42, nil }, MinMigration: 40}
	code, m := get(t, rd.Handler())
	if code != 200 || m["ok"] != true || m["migration"] != float64(42) {
		t.Fatalf("%d %v", code, m)
	}
}

func TestReadyFailsWhenDBDown(t *testing.T) {
	called := false
	rd := Readiness{
		Ping:             func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: connection refused") },
		MigrationVersion: func(context.Context) (int64, error) { called = true; return 1, nil },
	}
	code, m := get(t, rd.Handler())
	if code != http.StatusServiceUnavailable || m["ok"] != false || m["error"] != NotReadyMessage {
		t.Fatalf("%d %v", code, m)
	}
	if checks := m["checks"].(map[string]any); checks["db"] != "down" {
		t.Errorf("checks %v", checks)
	}
	if called {
		t.Error("migration check should be skipped when the database is down")
	}
	if raw, _ := json.Marshal(m); strings.Contains(string(raw), "10.0.0.5") {
		t.Error("internal error details leaked")
	}
}

func TestReadyFailsOnMigrations(t *testing.T) {
	behind := Readiness{Ping: okPing, MigrationVersion: func(context.Context) (int64, error) { return 3, nil }, MinMigration: 5}
	if code, m := get(t, behind.Handler()); code != 503 || m["checks"].(map[string]any)["migrations"] != "behind" {
		t.Fatalf("behind: %d %v", code, m)
	}
	broken := Readiness{Ping: okPing, MigrationVersion: func(context.Context) (int64, error) { return 0, errors.New("no table") }}
	if code, m := get(t, broken.Handler()); code != 503 || m["checks"].(map[string]any)["migrations"] != "unknown" {
		t.Fatalf("unknown: %d %v", code, m)
	}
}

func TestReadyTimeout(t *testing.T) {
	rd := Readiness{Timeout: 20 * time.Millisecond, Ping: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	if code, _ := get(t, rd.Handler()); code != 503 {
		t.Fatalf("hanging DB: %d", code)
	}
	if time.Since(start) > time.Second {
		t.Fatal("readiness check ignored its timeout")
	}
}

func TestReadyWithoutPingIsNotReady(t *testing.T) {
	if code, _ := get(t, Readiness{}.Handler()); code != 503 {
		t.Fatalf("unconfigured readiness reported ready: %d", code)
	}
}
