package config

import (
	"strings"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/platform/ratelimit"
)

func env(kv ...string) func(string) string {
	m := map[string]string{"DATABASE_URL": "postgres://x@127.0.0.1/x", "APP_ENV": "development"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

func TestRateDefaults(t *testing.T) {
	c, err := LoadFrom(env())
	if err != nil {
		t.Fatal(err)
	}
	perMin := func(l, b int) ratelimit.Rate { return ratelimit.Rate{Limit: l, Per: time.Minute, Burst: b} }
	want := ratelimit.Rates{Public: perMin(1200, 300), Write: perMin(120, 40), Upload: perMin(30, 10), Auth: perMin(60, 20),
		Demo: perMin(600, 100)}
	if c.Rates != want {
		t.Fatalf("rates = %+v\nwant  %+v", c.Rates, want)
	}
	if c.Auth.OTPMaxPerIPHour != 200 || c.Auth.OTPMaxSendsPerDay != 2000 || c.Auth.OTPMaxPerPhoneHour != 5 {
		t.Fatalf("otp limits = %+v", c.Auth)
	}
}

func TestRateOverrides(t *testing.T) {
	c, err := LoadFrom(env("RATE_PUBLIC_PER_MIN", "5000", "RATE_PUBLIC_BURST", "0", "RATE_WRITE_BURST", "7",
		"RATE_UPLOAD_PER_MIN", " 45 ", "RATE_AUTH_PER_MIN", "", "RATE_DEMO_PER_MIN", "900", "RATE_DEMO_BURST", "150",
		"OTP_MAX_SENDS_PER_DAY", "300"))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Rates
	if r.Public.Limit != 5000 || r.Public.Burst != 300 || r.Write.Limit != 120 || r.Write.Burst != 7 || r.Upload.Limit != 45 ||
		r.Auth.Limit != 60 || r.Demo.Limit != 900 || r.Demo.Burst != 150 {
		t.Fatalf("rates = %+v", r)
	}
	if c.Auth.OTPMaxSendsPerDay != 300 {
		t.Fatalf("OTP_MAX_SENDS_PER_DAY = %d", c.Auth.OTPMaxSendsPerDay)
	}
}

// Invalid values refuse to start, naming every bad variable at once.
func TestRateInvalidRefused(t *testing.T) {
	_, err := LoadFrom(env("RATE_PUBLIC_PER_MIN", "banyak", "RATE_WRITE_BURST", "-3", "RATE_DEMO_PER_MIN", "1.5",
		"RATE_AUTH_BURST", "2000000"))
	if err == nil {
		t.Fatal("invalid RATE_* accepted")
	}
	for _, k := range []string{"RATE_PUBLIC_PER_MIN", "RATE_WRITE_BURST", "RATE_DEMO_PER_MIN", "RATE_AUTH_BURST"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not name %s:\n%v", k, err)
		}
	}
	if _, err := LoadFrom(env("OTP_MAX_PER_IP_HOUR", "-1")); err == nil || !strings.Contains(err.Error(), "OTP_MAX_PER_IP_HOUR") {
		t.Fatalf("invalid OTP_MAX_PER_IP_HOUR: %v", err)
	}
}
