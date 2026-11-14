package config

import (
	"errors"
	"os"
	"strings"
)

type Config struct {
	Port        string
	DatabaseURL string
	AppEnv      string // demo | development | production
	DemoMode    bool   // fixed OTP, simulated eKYC/payment, demo login & clock. Never in production (PRD §13).
	UploadDir   string
	WebOrigin   string // used to fetch bundled demo photos for hashing (e.g. http://sewain-web:3000)
	CORSOrigins []string
}

func Load() (Config, error) {
	c := Config{
		Port:        env("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		AppEnv:      env("APP_ENV", "development"),
		DemoMode:    env("DEMO_MODE", "false") == "true",
		UploadDir:   env("UPLOAD_DIR", "./data/uploads"),
		WebOrigin:   strings.TrimRight(env("WEB_ORIGIN", "http://localhost:3000"), "/"),
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
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
