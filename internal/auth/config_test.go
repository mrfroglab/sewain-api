package auth

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestValidateConfigRejectsDevProvidersInProduction(t *testing.T) {
	cases := []struct {
		env      string
		demo     bool
		provider string
		ok       bool
	}{
		{"production", false, ProviderDemo, false},
		{"production", false, ProviderEcho, false},
		{"production", true, ProviderZenziva, false},
		{"production", false, ProviderZenziva, true},
		{"staging", false, ProviderEcho, true},
		{"staging", false, ProviderDemo, false}, // demo needs DEMO_MODE
		{"staging", true, ProviderDemo, true},
		{"demo", true, ProviderDemo, true},
		{"development", false, "twilio", false},
		{"development", false, "", false},
	}
	for _, c := range cases {
		err := ValidateConfig(c.env, c.demo, c.provider)
		if (err == nil) != c.ok {
			t.Errorf("ValidateConfig(%s, %v, %q) = %v, want ok=%v", c.env, c.demo, c.provider, err, c.ok)
		}
	}
}

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigFromEnv(t *testing.T) {
	pepper := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	if _, err := ConfigFromEnv(envMap(map[string]string{"APP_ENV": "production", "OTP_PROVIDER": "echo", "OTP_PEPPER": pepper})); err == nil {
		t.Fatal("echo in production must fail")
	}
	if _, err := ConfigFromEnv(envMap(map[string]string{"APP_ENV": "production", "DEMO_MODE": "true"})); err == nil {
		t.Fatal("demo mode in production must fail")
	}
	prod := map[string]string{
		"APP_ENV": "production", "OTP_PROVIDER": "zenziva", "OTP_PEPPER": pepper,
		"TERMS_VERSION": "1", "PRIVACY_VERSION": "1", "ALLOWED_ORIGINS": "https://sewain.me/, https://www.sewain.me",
		"ZENZIVA_WA_URL": "https://gateway.example/wa", "ZENZIVA_USERKEY": "u", "ZENZIVA_PASSKEY": "p",
	}
	c, err := ConfigFromEnv(envMap(prod))
	if err != nil {
		t.Fatal(err)
	}
	if !c.CookieSecure || c.AllowBearerLogin || len(c.AllowedOrigins) != 2 || c.AllowedOrigins[0] != "https://sewain.me" {
		t.Fatalf("prod config = %+v", c)
	}
	prod["SESSION_BEARER_LOGIN"] = "true"
	if _, err := ConfigFromEnv(envMap(prod)); err == nil {
		t.Fatal("bearer login in production must fail")
	}
	delete(prod, "SESSION_BEARER_LOGIN")
	delete(prod, "OTP_PEPPER")
	if _, err := ConfigFromEnv(envMap(prod)); err == nil {
		t.Fatal("missing pepper in production must fail")
	}
	d, err := ConfigFromEnv(envMap(map[string]string{"APP_ENV": "demo", "DEMO_MODE": "true"}))
	if err != nil || d.OTPProvider != ProviderDemo || !d.AllowBearerLogin || d.CookieSecure || len(d.OTPPepper) != 32 {
		t.Fatalf("demo config = %+v, %v", d, err)
	}
	if _, err := NewSender(Config{AppEnv: "production", OTPProvider: ProviderEcho}); err == nil {
		t.Fatal("NewSender(echo) in production must fail")
	}
}

func TestZenzivaSenderFallsBackToSMS(t *testing.T) {
	var waHits, smsHits atomic.Int32
	var smsForm url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/wa", func(w http.ResponseWriter, r *http.Request) {
		waHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/sms", func(w http.ResponseWriter, r *http.Request) {
		smsHits.Add(1)
		_ = r.ParseForm()
		smsForm = r.PostForm
		_, _ = io.WriteString(w, `{"status":"1","text":"Success"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	env := map[string]string{
		"ZENZIVA_WA_URL": srv.URL + "/wa", "ZENZIVA_SMS_URL": srv.URL + "/sms",
		"ZENZIVA_USERKEY": "key", "ZENZIVA_PASSKEY": "secret",
		"ZENZIVA_SUCCESS_FIELD": "status", "ZENZIVA_SUCCESS_VALUE": "1", "ZENZIVA_EXTRA": "brand=Sewain",
	}
	z, err := zenzivaConfigFromEnv(func(k, def string) string {
		if v, ok := env[k]; ok {
			return v
		}
		return def
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewZenzivaSender(z, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), OTPMessage{Phone: "+6281234567890", Code: "654321"}); err != nil {
		t.Fatal(err)
	}
	if waHits.Load() != 1 || smsHits.Load() != 1 {
		t.Fatalf("wa=%d sms=%d", waHits.Load(), smsHits.Load())
	}
	if smsForm.Get("to") != "081234567890" || !strings.Contains(smsForm.Get("message"), "654321") ||
		smsForm.Get("userkey") != "key" || smsForm.Get("passkey") != "secret" || smsForm.Get("brand") != "Sewain" {
		t.Fatalf("sms form = %v", smsForm)
	}
}

func TestZenzivaSenderJSONBasicAuthAndFailure(t *testing.T) {
	var gotUser, gotPass, gotCT string
	var status atomic.Value
	status.Store(`{"status":"1"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ = r.BasicAuth()
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "secret") {
			t.Error("credentials must not be in the body with basic auth")
		}
		_, _ = io.WriteString(w, status.Load().(string))
	}))
	defer srv.Close()
	cfg := ZenzivaConfig{WAURL: srv.URL, UserKey: "key", PassKey: "secret", Auth: "basic", Body: "json",
		FieldTo: "to", FieldMessage: "message", PhoneFormat: "62", SuccessField: "status", SuccessValue: "1"}
	s, err := NewZenzivaSender(cfg, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), OTPMessage{Phone: "+6281234567890", Code: "111222"}); err != nil {
		t.Fatal(err)
	}
	if gotUser != "key" || gotPass != "secret" || gotCT != "application/json" {
		t.Fatalf("auth=%s:%s ct=%s", gotUser, gotPass, gotCT)
	}
	status.Store(`{"status":"0","text":"saldo habis"}`)
	err = s.Send(context.Background(), OTPMessage{Phone: "+6281234567890", Code: "111222"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("want failure without credentials in error, got %v", err)
	}
}

func TestZenzivaConfigValidation(t *testing.T) {
	bad := []ZenzivaConfig{
		{},
		{WAURL: "ftp://x", UserKey: "k", Auth: "body", Body: "form", PhoneFormat: "local", Message: "{code}"},
		{WAURL: "https://x", UserKey: "k", Auth: "oauth", Body: "form", PhoneFormat: "local", Message: "{code}"},
		{WAURL: "https://x", UserKey: "k", Auth: "body", Body: "form", PhoneFormat: "local", Message: "no placeholder"},
		{WAURL: "https://x", Auth: "body", Body: "form", PhoneFormat: "local", Message: "{code}"},
	}
	for i, c := range bad {
		if err := c.validate(); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}
