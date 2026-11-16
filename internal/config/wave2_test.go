package config

import (
	"strings"
	"testing"
)

// prodEnv is a complete, valid production environment.
func prodEnv(kv ...string) func(string) string {
	base := []string{"APP_ENV", "production", "OTP_PROVIDER", "zenziva", "ZENZIVA_USERKEY", "u", "ZENZIVA_PASSKEY", "p", "ZENZIVA_WA_URL", "https://gateway.example/wa",
		"KYC_KEYS", "k1:" + strings.Repeat("A", 43) + "=", "NIK_PEPPER", strings.Repeat("B", 43) + "=",
		"OTP_PEPPER", strings.Repeat("C", 43) + "=", "MEDIA_SIGNING_KEY", strings.Repeat("D", 43) + "=",
		"CORS_ORIGINS", "https://sewain.example", "TERMS_VERSION", "1", "PRIVACY_VERSION", "1"}
	return env(append(base, kv...)...)
}

// Review wave 2 (C3): production accepted SESSION_COOKIE_SECURE=false, so the session cookie (D6) would also be sent
// over plain HTTP.
func TestProductionRefusesInsecureSessionCookie(t *testing.T) {
	if _, err := LoadFrom(prodEnv()); err != nil {
		t.Fatalf("baseline production env refused: %v", err)
	}
	_, err := LoadFrom(prodEnv("SESSION_COOKIE_SECURE", "false"))
	if err == nil || !strings.Contains(err.Error(), "SESSION_COOKIE_SECURE") {
		t.Fatalf("SESSION_COOKIE_SECURE=false in production: %v", err)
	}
}
