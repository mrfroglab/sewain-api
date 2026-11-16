package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/db/dbtest"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestProductionRefusesDemoConfig: APP_ENV=production with the demo/staging configuration does not start, and the
// error names every problem (before any database connection is attempted).
func TestProductionRefusesDemoConfig(t *testing.T) {
	for k, v := range map[string]string{
		"APP_ENV": "production", "DEMO_MODE": "true", "OTP_PROVIDER": "echo", "SNAPSHOT_SCOPE": "full", "PAYMENT_MODE": "simulated",
		"DATABASE_URL": "postgres://nobody@127.0.0.1:1/none", "KYC_KEYS": "", "NIK_PEPPER": "", "OTP_PEPPER": "", "MEDIA_SIGNING_KEY": "",
	} {
		t.Setenv(k, v)
	}
	err := cli(nil, io.Discard, discard())
	if err == nil {
		t.Fatal("production with demo configuration started")
	}
	for _, want := range []string{"DEMO_MODE=true is not allowed when APP_ENV=production", "OTP_PROVIDER=echo is not allowed",
		"SNAPSHOT_SCOPE=full is not allowed", "PAYMENT_MODE=simulated is not allowed", "KYC_KEYS is required", "NIK_PEPPER is required",
		"OTP_PEPPER is required", "MEDIA_SIGNING_KEY is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func secret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// startAndCheck runs the real serve path (migrate, wire, seed) on url like staging does today, waits for /api/ready,
// checks a snapshot, then shuts down gracefully.
func startAndCheck(t *testing.T, url string) {
	t.Helper()
	port := freePort(t)
	env := map[string]string{"DATABASE_URL": url, "PORT": port, "APP_ENV": "staging", "DEMO_MODE": "true", "OTP_PROVIDER": "echo",
		"SNAPSHOT_SCOPE": "full", "METRICS_ADDR": "off", "UPLOAD_DIR": t.TempDir(), "OTP_PEPPER": secret(), "NIK_PEPPER": secret(),
		"KYC_KEYS": "v1:" + secret(), "MEDIA_SIGNING_KEY": secret(), "RETENTION_INTERVAL": "1h"}
	t.Setenv("UPLOAD_DIR", env["UPLOAD_DIR"])
	t.Setenv("MEDIA_SIGNING_KEY", env["MEDIA_SIGNING_KEY"])
	cfg, err := config.LoadFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, discard()) }()
	base := "http://127.0.0.1:" + port
	deadline := time.Now().Add(60 * time.Second)
	for {
		res, err := http.Get(base + "/api/ready")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("server stopped: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server not ready")
		}
		time.Sleep(200 * time.Millisecond)
	}
	res, err := http.Get(base + "/api/v1/state")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("state: %v %v", err, res)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `"u-raka"`) {
		t.Fatal("demo seed missing")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("graceful shutdown hung")
	}
}

func TestServeOnEmptyDatabase(t *testing.T) { startAndCheck(t, dbtest.NewDB(t)) }

// TestServeOnV1Database: the staging database still holds the v1 JSONB schema; the v2 binary migrates over it.
func TestServeOnV1Database(t *testing.T) {
	url := dbtest.NewDB(t)
	v1, err := os.ReadFile("../../internal/db/testdata/v1_001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(v1)); err != nil {
		t.Fatal(err)
	}
	_, _ = pool.Exec(context.Background(), `INSERT INTO users (id, data) VALUES ('u-old', '{"phone":"0811","role":"user","status":"aktif","joinedAt":1}')`)
	pool.Close()
	startAndCheck(t, url)
}

func TestCLIVapidAndAdmin(t *testing.T) {
	var out strings.Builder
	if err := cli([]string{"vapid", "gen"}, &out, discard()); err != nil || !strings.Contains(out.String(), "VAPID_PUBLIC_KEY=B") ||
		!strings.Contains(out.String(), "VAPID_PRIVATE_KEY=") {
		t.Fatalf("vapid gen: %v %q", err, out.String())
	}
	url := dbtest.NewDB(t)
	t.Setenv("DATABASE_URL", url)
	if err := cli([]string{"migrate"}, io.Discard, discard()); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	_, err = pool.Exec(context.Background(), `INSERT INTO users (id, phone, name, area_id, joined_at) VALUES ('u-janu', '081234560000', 'Janu', 'ugm', now())`)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli([]string{"admin", "grant", "+62 812-3456-0000"}, io.Discard, discard()); err != nil {
		t.Fatal(err)
	}
	var role string
	_ = pool.QueryRow(context.Background(), `SELECT role FROM users WHERE id = 'u-janu'`).Scan(&role)
	if role != "admin" {
		t.Fatalf("grant: %s", role)
	}
	if err := cli([]string{"admin", "revoke", "081234560000"}, io.Discard, discard()); err != nil {
		t.Fatal(err)
	}
	if err := cli([]string{"admin", "grant", "081299999999"}, io.Discard, discard()); err == nil {
		t.Fatal("unknown phone must fail")
	}
}
