package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mrfroglab/sewain-api/internal/crypto"
)

// OTP provider names (OTP_PROVIDER).
const (
	ProviderDemo    = "demo"    // fixed code 123456, nothing sent; DEMO_MODE only
	ProviderEcho    = "echo"    // code returned in the API response; staging only
	ProviderZenziva = "zenziva" // WhatsApp, SMS fallback
)

// Tunables (ROADMAP F2.1/F2.2).
const (
	OTPTTL             = 5 * time.Minute
	OTPMaxAttempts     = 5
	OTPResendGap       = 60 * time.Second
	OTPMaxPerPhoneHour = 5
	OTPMaxPerIPHour    = 20
	SessionTTL         = 30 * 24 * time.Hour
	SessionTouchEvery  = 5 * time.Minute
	SessionCookieName  = "sewain_session"
	DemoOTPCode        = "123456"
)

// Config is the auth package configuration. Load it with ConfigFromEnv or fill it directly.
type Config struct {
	AppEnv         string // demo | development | staging | production
	DemoMode       bool
	OTPProvider    string
	OTPPepper      []byte
	CookieSecure   bool
	AllowedOrigins []string // exact origins (scheme://host[:port]) accepted for cookie-based mutations
	TermsVersion   string
	PrivacyVersion string
	// AllowBearerLogin lets OTP verify return the session token in the body (transport "bearer") instead of
	// only setting the HttpOnly cookie. Defaults to DemoMode (multi-account-per-tab demo, D6).
	AllowBearerLogin bool
	// ClientIPHeader, when set (e.g. "CF-Connecting-IP"), is trusted for the client IP used in rate limits
	// and session metadata. Only set it behind a proxy that overwrites the header.
	ClientIPHeader string
	Zenziva        ZenzivaConfig
}

// ValidateConfig rejects OTP providers that must never run in production (D10) and unknown providers.
func ValidateConfig(appEnv string, demoMode bool, provider string) error {
	prod := appEnv == "production"
	if prod && demoMode {
		return errors.New("auth: DEMO_MODE=true is not allowed when APP_ENV=production")
	}
	switch provider {
	case ProviderDemo:
		if prod {
			return errors.New("auth: OTP_PROVIDER=demo is not allowed when APP_ENV=production")
		}
		if !demoMode {
			return errors.New("auth: OTP_PROVIDER=demo requires DEMO_MODE=true")
		}
	case ProviderEcho:
		if prod {
			return errors.New("auth: OTP_PROVIDER=echo is not allowed when APP_ENV=production")
		}
	case ProviderZenziva:
	case "":
		return errors.New("auth: OTP_PROVIDER is required (demo|echo|zenziva)")
	default:
		return fmt.Errorf("auth: unknown OTP_PROVIDER %q (demo|echo|zenziva)", provider)
	}
	return nil
}

// ConfigFromEnv reads the auth environment variables (see docs/integration/auth.md) and validates them.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	get := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		AppEnv:         get("APP_ENV", "development"),
		DemoMode:       get("DEMO_MODE", "false") == "true",
		TermsVersion:   get("TERMS_VERSION", ""),
		PrivacyVersion: get("PRIVACY_VERSION", ""),
		ClientIPHeader: get("CLIENT_IP_HEADER", ""),
	}
	defProvider := ""
	switch {
	case c.DemoMode:
		defProvider = ProviderDemo
	case c.AppEnv == "development":
		defProvider = ProviderEcho
	}
	c.OTPProvider = get("OTP_PROVIDER", defProvider)
	c.CookieSecure = get("SESSION_COOKIE_SECURE", fmt.Sprint(c.AppEnv != "development" && c.AppEnv != "demo")) == "true"
	c.AllowBearerLogin = get("SESSION_BEARER_LOGIN", fmt.Sprint(c.DemoMode)) == "true"
	for _, o := range strings.Split(get("ALLOWED_ORIGINS", ""), ",") {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			c.AllowedOrigins = append(c.AllowedOrigins, o)
		}
	}
	if err := ValidateConfig(c.AppEnv, c.DemoMode, c.OTPProvider); err != nil {
		return c, err
	}
	if c.AppEnv == "production" && c.AllowBearerLogin {
		return c, errors.New("auth: SESSION_BEARER_LOGIN=true is not allowed when APP_ENV=production")
	}
	pepper := getenv("OTP_PEPPER")
	switch {
	case pepper != "":
		p, err := crypto.DecodeSecret(pepper)
		if err != nil {
			return c, fmt.Errorf("auth: OTP_PEPPER: %w", err)
		}
		c.OTPPepper = p
	case c.AppEnv == "production" || c.AppEnv == "staging":
		return c, errors.New("auth: OTP_PEPPER is required outside development/demo")
	default:
		// Dev/demo convenience: an ephemeral pepper only invalidates outstanding OTPs on restart.
		p, err := crypto.RandomBytes(32)
		if err != nil {
			return c, err
		}
		c.OTPPepper = p
	}
	if c.TermsVersion == "" || c.PrivacyVersion == "" {
		if c.AppEnv == "production" {
			return c, errors.New("auth: TERMS_VERSION and PRIVACY_VERSION are required in production")
		}
		if c.TermsVersion == "" {
			c.TermsVersion = "draft"
		}
		if c.PrivacyVersion == "" {
			c.PrivacyVersion = "draft"
		}
	}
	if len(c.AllowedOrigins) == 0 && c.AppEnv == "production" {
		return c, errors.New("auth: ALLOWED_ORIGINS is required in production")
	}
	if c.OTPProvider == ProviderZenziva {
		z, err := zenzivaConfigFromEnv(get)
		if err != nil {
			return c, err
		}
		c.Zenziva = z
	}
	return c, nil
}

// NewSender builds the Sender for c.OTPProvider.
func NewSender(c Config) (Sender, error) {
	if err := ValidateConfig(c.AppEnv, c.DemoMode, c.OTPProvider); err != nil {
		return nil, err
	}
	switch c.OTPProvider {
	case ProviderDemo:
		return DemoSender{}, nil
	case ProviderEcho:
		return EchoSender{}, nil
	default:
		return NewZenzivaSender(c.Zenziva, nil)
	}
}
