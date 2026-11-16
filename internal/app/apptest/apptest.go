// Package apptest runs the whole API (internal/app) against a fresh test database for end-to-end HTTP tests. Tests
// are skipped when TEST_DATABASE_URL is empty (see internal/db/dbtest).
package apptest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/app"
	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/db/dbtest"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/httpapi"
	"github.com/mrfroglab/sewain-api/internal/media"
	"github.com/mrfroglab/sewain-api/internal/platform/ratelimit"
)

// Clock is a settable real clock (the demo clock adds its own offset on top).
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

// Add moves the clock.
func (c *Clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// Options select the environment.
type Options struct {
	// Demo runs APP_ENV=demo with DEMO_MODE and the demo seed. Otherwise APP_ENV=staging, OTP echo, real keys,
	// PAYMENT_MODE=offline and an empty database.
	Demo bool
	// Scope is SNAPSHOT_SCOPE (default scoped).
	Scope string
	// Env adds or overrides environment variables.
	Env map[string]string
	// RealRateLimits keeps the production rate-limit profiles (default: effectively unlimited).
	RealRateLimits bool
}

// API is a running server.
type API struct {
	T     *testing.T
	Base  string
	App   *app.App
	Clock *Clock
	Web   *httptest.Server
}

// Origin is the web origin allowed by CORS/CSRF in tests.
const Origin = "http://localhost:3000"

func secret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// New starts the API.
func New(t *testing.T, opt Options) *API {
	t.Helper()
	pool := dbtest.NewPool(t)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/img/") {
			_, _ = w.Write(PNG(t, 3))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(web.Close)
	env := map[string]string{
		"DATABASE_URL": pool.Config().ConnString(), "METRICS_ADDR": "off", "UPLOAD_DIR": t.TempDir(), "WEB_ORIGIN": web.URL,
		"CORS_ORIGINS": Origin, "SNAPSHOT_SCOPE": "scoped",
	}
	if opt.Demo {
		env["APP_ENV"], env["DEMO_MODE"] = "demo", "true"
	} else {
		env["APP_ENV"], env["DEMO_MODE"], env["OTP_PROVIDER"] = "staging", "false", "echo"
		env["OTP_PEPPER"], env["NIK_PEPPER"], env["KYC_KEYS"], env["MEDIA_SIGNING_KEY"] = secret(), secret(), "v1:"+secret(), secret()
		env["TERMS_VERSION"], env["PRIVACY_VERSION"] = "2026-11-test", "2026-11-test"
		env["SESSION_COOKIE_SECURE"] = "false"
	}
	if opt.Scope != "" {
		env["SNAPSHOT_SCOPE"] = opt.Scope
	}
	for k, v := range opt.Env {
		env[k] = v
	}
	cfg, err := config.LoadFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	clock := &Clock{t: time.Now()}
	store, err := media.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	signer, err := media.NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	listening := make(chan struct{}, 1)
	ao := app.Options{Storage: store, Now: clock.Now, Signer: signer, PushHosts: []string{"*"},
		OnListen: func() {
			select {
			case listening <- struct{}{}:
			default:
			}
		}}
	if !opt.RealRateLimits {
		big := ratelimit.Rate{Limit: 1_000_000, Per: time.Minute, Burst: 1_000_000}
		ao.RateProfiles = httpapi.RateProfiles{
			Public: ratelimit.Profile{Name: "public", Rate: big, Key: ratelimit.ByIP},
			Write:  ratelimit.Profile{Name: "write", Rate: big, Key: ratelimit.ByUser},
			Upload: ratelimit.Profile{Name: "upload", Rate: big, Key: ratelimit.ByUser},
			Auth:   ratelimit.Profile{Name: "auth", Rate: big, Key: ratelimit.ByIP},
		}
	}
	logOut := io.Writer(io.Discard)
	if os.Getenv("APPTEST_LOG") != "" {
		logOut = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logOut, nil))
	a, err := app.New(cfg, pool, log, ao)
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Listen(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not start")
	}
	if opt.Demo {
		if _, err := a.Svc.SeedIfEmpty(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() { a.Close(); srv.Close() })
	return &API{T: t, Base: srv.URL, App: a, Clock: clock, Web: web}
}

// PNG is a tiny valid image whose bytes depend on seed.
func PNG(t testing.TB, seed uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	img.Set(1, 1, color.RGBA{R: seed, G: 7, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Session is how a client authenticates: a bearer token (demo, per tab) or a session cookie (production).
type Session struct {
	Token  string
	Cookie string
	UserID string
}

// Resp is a decoded response.
type Resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

// Req builds a request.
type Req struct {
	Method, Path string
	JSON         any
	Body         io.Reader
	ContentType  string
	Headers      map[string]string
	// NoOrigin leaves out the Origin header that browsers send with cookie mutations.
	NoOrigin bool
}

// Do sends r as s (nil = guest).
func (a *API) Do(s *Session, r Req) Resp {
	a.T.Helper()
	body := r.Body
	ct := r.ContentType
	if r.JSON != nil {
		b, _ := json.Marshal(r.JSON)
		body, ct = bytes.NewReader(b), "application/json"
	}
	req, err := http.NewRequest(r.Method, a.Base+r.Path, body)
	if err != nil {
		a.T.Fatal(err)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if s != nil && s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	if s != nil && s.Cookie != "" {
		req.AddCookie(&http.Cookie{Name: "sewain_session", Value: s.Cookie, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
		if !r.NoOrigin {
			req.Header.Set("Origin", Origin)
		}
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.T.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := Resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

// Get is a GET as s.
func (a *API) Get(s *Session, path string) Resp { return a.Do(s, Req{Method: "GET", Path: path}) }

// DemoLogin signs in as a seed user (DEMO_MODE, bearer).
func (a *API) DemoLogin(userID string) *Session {
	a.T.Helper()
	r := a.Do(nil, Req{Method: "POST", Path: "/api/v1/auth/demo-login", JSON: map[string]any{"userId": userID}})
	tok, _ := r.Body["token"].(string)
	if tok == "" {
		a.T.Fatalf("demo login %s: %d %s", userID, r.Status, r.Raw)
	}
	return &Session{Token: tok, UserID: userID}
}

// OTPLogin signs in (and signs up) through OTP with the echo provider and a session cookie.
func (a *API) OTPLogin(phone string) *Session {
	a.T.Helper()
	r := a.Do(nil, Req{Method: "POST", Path: "/api/v1/auth/otp/request", JSON: map[string]any{"phone": phone}})
	code, _ := r.Body["devCode"].(string)
	if r.Status != 200 || code == "" {
		a.T.Fatalf("otp request: %d %s", r.Status, r.Raw)
	}
	v := a.Do(nil, Req{Method: "POST", Path: "/api/v1/auth/otp/verify", JSON: map[string]any{"phone": phone, "code": code, "acceptTerms": true}})
	if v.Status != 200 {
		a.T.Fatalf("otp verify: %d %s", v.Status, v.Raw)
	}
	s := &Session{UserID: v.Body["userId"].(string)}
	for _, c := range (&http.Response{Header: v.Header}).Cookies() {
		if c.Name == "sewain_session" {
			s.Cookie = c.Value
		}
	}
	if s.Cookie == "" {
		a.T.Fatalf("otp verify set no cookie: %v", v.Header)
	}
	return s
}

// Act runs an action (args[0] is the acting user).
func (a *API) Act(s *Session, name string, args ...any) Resp {
	a.T.Helper()
	return a.Do(s, Req{Method: "POST", Path: "/api/v1/actions/" + name, JSON: map[string]any{"args": args}})
}

// OK runs an action that must succeed and returns its value.
func (a *API) OK(s *Session, name string, args ...any) any {
	a.T.Helper()
	r := a.Act(s, name, args...)
	if r.Status != 200 || r.Body["ok"] != true {
		a.T.Fatalf("%s: %d %s", name, r.Status, r.Raw)
	}
	return r.Body["value"]
}

// State is the snapshot as s.
func (a *API) State(s *Session) *domain.Data {
	a.T.Helper()
	r := a.Get(s, "/api/v1/state")
	if r.Status != 200 {
		a.T.Fatalf("state: %d %s", r.Status, r.Raw)
	}
	var d domain.Data
	if err := json.Unmarshal(r.Raw, &d); err != nil {
		a.T.Fatal(err)
	}
	return &d
}

// Upload sends a photo (multipart) and returns the response.
func (a *API) Upload(s *Session, stage, bookingID string, data []byte) Resp {
	a.T.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "x.png")
	_, _ = fw.Write(data)
	_ = mw.WriteField("stage", stage)
	if bookingID != "" {
		_ = mw.WriteField("bookingId", bookingID)
	}
	_ = mw.WriteField("source", "kamera")
	_ = mw.WriteField("angle", "Depan")
	_ = mw.Close()
	return a.Do(s, Req{Method: "POST", Path: "/api/v1/photos", Body: &body, ContentType: mw.FormDataContentType()})
}

// Multipart builds a multipart body from fields and files (name → bytes).
func Multipart(fields map[string]string, files map[string][]byte) (io.Reader, string) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for k, v := range files {
		fw, _ := mw.CreateFormFile(k, k+".png")
		_, _ = fw.Write(v)
	}
	_ = mw.Close()
	return &body, mw.FormDataContentType()
}

// ISO is a WIB date days from now.
func ISO(days int) string { return time.Now().AddDate(0, 0, days).In(domain.WIB).Format("2006-01-02") }
