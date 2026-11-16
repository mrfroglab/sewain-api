// Package config reads the environment of the API process and refuses unsafe combinations at start (PRD §19 risk
// table, ROADMAP D10): production never runs with demo features, echo/demo OTP, the full snapshot or missing keys.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mrfroglab/sewain-api/internal/auth"
	"github.com/mrfroglab/sewain-api/internal/kyc"
	"github.com/mrfroglab/sewain-api/internal/notify"
	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
	"github.com/mrfroglab/sewain-api/internal/platform/ratelimit"
)

// App environments (APP_ENV).
const (
	EnvDevelopment = "development"
	EnvDemo        = "demo"
	EnvStaging     = "staging"
	EnvProduction  = "production"
)

// Config is the whole process configuration. Module settings are parsed by their packages.
type Config struct {
	Port        string
	DatabaseURL string
	AppEnv      string // demo | development | staging | production
	DemoMode    bool   // demo login, simulated eKYC/payment, demo clock + reset. Never in production (PRD §13).
	UploadDir   string
	WebOrigin   string // used to fetch bundled demo photos for hashing (e.g. http://sewain-web:3000)
	CORSOrigins []string
	// SnapshotScope: "scoped" (default, per viewer, ROADMAP §4) or "full" (v1 behavior, DEMO_MODE only).
	SnapshotScope string
	// SnapshotUsers: which public profiles a scoped (non-admin) snapshot carries. "referenced" (default) = only
	// users the rest of the viewer's snapshot points at (listing owners, booking parties, review authors/targets,
	// wanted board, ...), plus every demo persona in DEMO_MODE; "all" = every user. See
	// docs/loadtest/2026-11-report.md.
	SnapshotUsers string
	// MigrateOnStart applies pending goose migrations at startup (under an advisory lock). Default true.
	MigrateOnStart bool
	// PaymentMode: simulated (default with DEMO_MODE) | offline (default otherwise) | gateway (Fase 8, refused).
	PaymentMode string

	// Platform (ops).
	MetricsAddr       string // "" = metrics server off
	TrustedProxyCIDRs []string
	RequestTimeout    time.Duration
	LogLevel          string
	RetentionEvery    time.Duration // retention runner interval
	// Rates are the HTTP rate-limit profiles (RATE_<PROFILE>_PER_MIN / RATE_<PROFILE>_BURST), defaults filled in.
	Rates ratelimit.Rates

	// Media.
	MediaBackend    string // local | s3
	MediaSigningKey bool   // MEDIA_SIGNING_KEY is set (the media package reads the value itself)

	Auth   auth.Config
	KYC    kyc.Config
	Notify notify.Config
}

// Load reads the process environment.
func Load() (Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads the configuration through getenv and validates it. Every problem is reported at once so an
// operator can fix the environment in one pass.
func LoadFrom(getenv func(string) string) (Config, error) {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		Port:           get("PORT", "8080"),
		DatabaseURL:    get("DATABASE_URL", ""),
		AppEnv:         get("APP_ENV", EnvDevelopment),
		DemoMode:       get("DEMO_MODE", "false") == "true",
		UploadDir:      get("UPLOAD_DIR", "./data/uploads"),
		WebOrigin:      strings.TrimRight(get("WEB_ORIGIN", "http://localhost:3000"), "/"),
		SnapshotScope:  get("SNAPSHOT_SCOPE", "scoped"),
		SnapshotUsers:  get("SNAPSHOT_USERS", "referenced"),
		MigrateOnStart: get("MIGRATE_ON_START", "true") != "false",
		PaymentMode:    get("PAYMENT_MODE", ""),
		MetricsAddr:    get("METRICS_ADDR", "127.0.0.1:9090"),
		LogLevel:       get("LOG_LEVEL", "info"),
		MediaBackend:   strings.ToLower(get("MEDIA_BACKEND", "local")),
	}
	if c.MetricsAddr == "off" {
		c.MetricsAddr = ""
	}
	c.TrustedProxyCIDRs = middleware.ParseCIDRList(getenv("TRUSTED_PROXY_CIDRS"))
	for _, o := range strings.Split(get("CORS_ORIGINS", "http://localhost:3000"), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.CORSOrigins = append(c.CORSOrigins, o)
		}
	}
	if c.PaymentMode == "" {
		c.PaymentMode = "offline"
		if c.DemoMode {
			c.PaymentMode = "simulated"
		}
	}
	c.MediaSigningKey = strings.TrimSpace(getenv("MEDIA_SIGNING_KEY")) != ""

	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	duration := func(key, def string) time.Duration {
		d, err := time.ParseDuration(get(key, def))
		if err != nil || d <= 0 {
			fail("%s must be a positive duration like %s", key, def)
			d, _ = time.ParseDuration(def)
		}
		return d
	}
	c.RequestTimeout = duration("REQUEST_TIMEOUT", "15s")
	c.RetentionEvery = duration("RETENTION_INTERVAL", "1h")
	c.Rates = rates(getenv, fail)

	prod := c.AppEnv == EnvProduction
	switch c.AppEnv {
	case EnvDevelopment, EnvDemo, EnvStaging, EnvProduction:
	default:
		fail("APP_ENV must be development, demo, staging or production (got %q)", c.AppEnv)
	}
	if c.DatabaseURL == "" {
		fail("DATABASE_URL is required")
	}
	// PRD risk table: "DEMO_MODE terbawa ke produksi" → refuse to start.
	if c.DemoMode && prod {
		fail("DEMO_MODE=true is not allowed when APP_ENV=production")
	}
	switch c.SnapshotScope {
	case "scoped":
	case "full":
		if prod {
			fail("SNAPSHOT_SCOPE=full is not allowed when APP_ENV=production (every user would see everyone's data)")
		} else if !c.DemoMode {
			fail("SNAPSHOT_SCOPE=full is only allowed with DEMO_MODE=true")
		}
	default:
		fail(`SNAPSHOT_SCOPE must be "scoped" or "full"`)
	}
	if c.SnapshotUsers != "all" && c.SnapshotUsers != "referenced" {
		fail(`SNAPSHOT_USERS must be "all" or "referenced"`)
	}
	switch c.PaymentMode {
	case "offline":
	case "simulated":
		if prod {
			fail("PAYMENT_MODE=simulated is not allowed when APP_ENV=production (use offline)")
		}
	case "gateway":
		fail("PAYMENT_MODE=gateway is reserved for Fase 8 and not available yet (use offline)")
	default:
		fail("PAYMENT_MODE must be simulated, offline or gateway (got %q)", c.PaymentMode)
	}
	if prod || c.AppEnv == EnvStaging {
		// Keys must be real and stable outside dev/demo: ephemeral keys would make encrypted KYC data, signed
		// photo URLs and OTP codes unusable after a restart.
		for _, k := range []string{"KYC_KEYS", "NIK_PEPPER", "OTP_PEPPER", "MEDIA_SIGNING_KEY"} {
			if strings.TrimSpace(getenv(k)) == "" {
				fail("%s is required when APP_ENV=%s (generate it: see README \"Kunci\")", k, c.AppEnv)
			}
		}
	}
	if prod {
		if otp := get("OTP_PROVIDER", ""); otp == auth.ProviderDemo || otp == auth.ProviderEcho {
			fail("OTP_PROVIDER=%s is not allowed when APP_ENV=production (use zenziva)", otp)
		}
		for _, o := range c.CORSOrigins {
			if o == "*" {
				fail("CORS_ORIGINS=* is not allowed when APP_ENV=production")
			}
		}
		// D6: the session cookie is Secure in production (auth only turns it off when asked to).
		if v := get("SESSION_COOKIE_SECURE", "true"); v != "true" {
			fail("SESSION_COOKIE_SECURE=%s is not allowed when APP_ENV=production (the session cookie must be Secure)", v)
		}
	}
	switch c.MediaBackend {
	case "local", "s3", "r2":
	default:
		fail("MEDIA_BACKEND must be local or s3 (got %q)", c.MediaBackend)
	}
	if len(errs) > 0 {
		// Module parsers would only repeat the same problems.
		return c, configError(errs)
	}

	// Modules. ALLOWED_ORIGINS (cookie CSRF check) defaults to CORS_ORIGINS without "*".
	authEnv := func(k string) string {
		if k == "ALLOWED_ORIGINS" && strings.TrimSpace(getenv(k)) == "" {
			var origins []string
			for _, o := range c.CORSOrigins {
				if o != "*" {
					origins = append(origins, o)
				}
			}
			return strings.Join(origins, ",")
		}
		return getenv(k)
	}
	var err error
	if c.Auth, err = auth.ConfigFromEnv(authEnv); err != nil {
		errs = append(errs, err)
	}
	// The client IP comes from the trusted-proxy aware resolver (TRUSTED_PROXY_CIDRS); a bare header would be
	// spoofable when the API is reached without the proxy.
	c.Auth.ClientIPHeader = ""
	if c.KYC, err = kyc.ConfigFromEnv(getenv); err != nil {
		errs = append(errs, err)
	}
	if c.Notify, err = notify.ConfigFromEnv(getenv); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return c, configError(errs)
	}
	return c, nil
}

// MaxRateValue bounds RATE_* values: anything larger is a typo, not a limit.
const MaxRateValue = 1_000_000

// rates reads RATE_<PROFILE>_PER_MIN and RATE_<PROFILE>_BURST for every built-in profile. Empty or 0 keeps the
// default; anything else must be a whole number between 1 and MaxRateValue.
func rates(getenv func(string) string, fail func(string, ...any)) ratelimit.Rates {
	r := ratelimit.DefaultRates()
	for _, p := range []struct {
		env  string
		rate *ratelimit.Rate
	}{{"IP", &r.IP}, {"PUBLIC", &r.Public}, {"WRITE", &r.Write}, {"UPLOAD", &r.Upload}, {"AUTH", &r.Auth}, {"DEMO", &r.Demo}} {
		if n, ok := PositiveInt(getenv, "RATE_"+p.env+"_PER_MIN", MaxRateValue, fail); ok {
			p.rate.Limit = n
		}
		if n, ok := PositiveInt(getenv, "RATE_"+p.env+"_BURST", MaxRateValue, fail); ok {
			p.rate.Burst = n
		}
	}
	return r
}

// PositiveInt reads an optional whole-number setting. It returns ok=false when the variable is empty or 0 (use
// the default) and reports anything that is not a whole number in [1, upper] through fail.
func PositiveInt(getenv func(string) string, key string, upper int, fail func(string, ...any)) (int, bool) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	switch {
	case err != nil || n < 0 || n > upper:
		fail("%s must be a whole number between 1 and %d, or empty/0 for the default (got %q)", key, upper, v)
		return 0, false
	case n == 0:
		return 0, false
	}
	return n, true
}

func configError(errs []error) error {
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = "  - " + e.Error()
	}
	return errors.New("invalid configuration:\n" + strings.Join(msgs, "\n"))
}

// Production reports APP_ENV=production.
func (c Config) Production() bool { return c.AppEnv == EnvProduction }

// EphemeralKeysOK reports whether missing keys may be replaced by random ones (development and demo only).
func (c Config) EphemeralKeysOK() bool { return c.AppEnv == EnvDevelopment || c.AppEnv == EnvDemo }
