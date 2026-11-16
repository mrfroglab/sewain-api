package config

import (
	"errors"
	"os"
	"strings"
)

type Config struct {
	Port        string
	DatabaseURL string
	AppEnv      string // demo | development | staging | production
	DemoMode    bool   // demo login, simulated eKYC/payment, demo clock + reset. Never in production (PRD §13).
	UploadDir   string
	WebOrigin   string // used to fetch bundled demo photos for hashing (e.g. http://sewain-web:3000)
	CORSOrigins []string
	// SnapshotScope: "scoped" (default, per viewer, ROADMAP §4) or "full" (v1 behaviour, DEMO_MODE only).
	SnapshotScope string
	// MigrateOnStart applies pending goose migrations at startup (under an advisory lock). Default true.
	MigrateOnStart bool
}

func Load() (Config, error) {
	c := Config{
		Port:           env("PORT", "8080"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		AppEnv:         env("APP_ENV", "development"),
		DemoMode:       env("DEMO_MODE", "false") == "true",
		UploadDir:      env("UPLOAD_DIR", "./data/uploads"),
		WebOrigin:      strings.TrimRight(env("WEB_ORIGIN", "http://localhost:3000"), "/"),
		SnapshotScope:  env("SNAPSHOT_SCOPE", "scoped"),
		MigrateOnStart: env("MIGRATE_ON_START", "true") != "false",
	}
	for _, o := range strings.Split(env("CORS_ORIGINS", "http://localhost:3000"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	// PRD risk table: "DEMO_MODE terbawa ke produksi" → refuse to start.
	if c.DemoMode && c.AppEnv == "production" {
		return c, errors.New("DEMO_MODE=true is not allowed when APP_ENV=production")
	}
	switch c.SnapshotScope {
	case "scoped":
	case "full":
		if !c.DemoMode {
			return c, errors.New("SNAPSHOT_SCOPE=full is only allowed with DEMO_MODE=true")
		}
	default:
		return c, errors.New(`SNAPSHOT_SCOPE must be "scoped" or "full"`)
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
