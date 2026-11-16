package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mrfroglab/sewain-api/internal/auth"
	"github.com/mrfroglab/sewain-api/internal/auth/authtest"
)

var t0 = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

type fixture struct {
	db    *pgxpool.Pool
	svc   *auth.Service
	clock *authtest.Clock
	dir   *authtest.Directory
}

func newFixture(t *testing.T, sender auth.Sender, mutate ...func(*auth.Config)) *fixture {
	t.Helper()
	db := authtest.DB(t)
	cfg := auth.Config{
		AppEnv: "staging", OTPProvider: sender.Name(), OTPPepper: []byte(strings.Repeat("p", 32)),
		TermsVersion: "2026-11", PrivacyVersion: "2026-11a", AllowedOrigins: []string{"https://sewain.example"},
		CookieSecure: true, AllowBearerLogin: true,
	}
	if sender.Name() == auth.ProviderDemo {
		cfg.DemoMode = true
	}
	for _, m := range mutate {
		m(&cfg)
	}
	dir := &authtest.Directory{DB: db}
	svc, err := auth.NewService(db, dir, sender, cfg)
	if err != nil {
		t.Fatal(err)
	}
	clock := authtest.NewClock(t0)
	svc.Now = clock.Now
	return &fixture{db: db, svc: svc, clock: clock, dir: dir}
}

func wantStatus(t *testing.T, err error, status int) *auth.Error {
	t.Helper()
	var ue *auth.Error
	if !errors.As(err, &ue) || ue.Status != status {
		t.Fatalf("want *auth.Error with status %d, got %v", status, err)
	}
	return ue
}

var ctx = context.Background()

func (f *fixture) request(t *testing.T, phone, ip string) auth.RequestResult {
	t.Helper()
	res, err := f.svc.RequestOTP(ctx, phone, auth.RequestMeta{IP: ip})
	if err != nil {
		t.Fatalf("RequestOTP(%s, %s): %v", phone, ip, err)
	}
	return res
}

func (f *fixture) login(t *testing.T, phone string) auth.VerifyResult {
	t.Helper()
	res := f.request(t, phone, "10.0.0.1")
	v, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: res.DevCode, AcceptTerms: true}, auth.RequestMeta{IP: "10.0.0.1", UserAgent: "test"})
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}
	return v
}

func TestOTPResendGapAndPerPhoneLimit(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	phone := "081234567890"
	f.request(t, phone, "1.1.1.1")
	_, err := f.svc.RequestOTP(ctx, phone, auth.RequestMeta{IP: "1.1.1.2"})
	ue := wantStatus(t, err, http.StatusTooManyRequests)
	if ue.RetryAfter <= 0 || ue.RetryAfter > auth.OTPResendGap {
		t.Fatalf("retryAfter = %v", ue.RetryAfter)
	}
	for i := 2; i <= auth.OTPMaxPerPhoneHour; i++ {
		f.clock.Advance(auth.OTPResendGap + time.Second)
		f.request(t, phone, fmt.Sprintf("1.1.1.%d", i))
	}
	f.clock.Advance(auth.OTPResendGap + time.Second)
	_, err = f.svc.RequestOTP(ctx, phone, auth.RequestMeta{IP: "9.9.9.9"})
	ue = wantStatus(t, err, http.StatusTooManyRequests)
	if !strings.Contains(ue.Msg, "nomor ini") {
		t.Fatalf("msg = %q", ue.Msg)
	}
	// The window slides: an hour after the first request one slot frees up.
	f.clock.Set(t0.Add(time.Hour + time.Second))
	f.request(t, phone, "9.9.9.9")
}

func TestOTPPerIPLimit(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	for i := 0; i < auth.OTPMaxPerIPHour; i++ {
		f.request(t, fmt.Sprintf("08123400%04d", i), "5.5.5.5")
	}
	_, err := f.svc.RequestOTP(ctx, "081299990000", auth.RequestMeta{IP: "5.5.5.5"})
	ue := wantStatus(t, err, http.StatusTooManyRequests)
	if !strings.Contains(ue.Msg, "jaringanmu") {
		t.Fatalf("msg = %q", ue.Msg)
	}
	f.request(t, "081299990000", "5.5.5.6")
}

func TestOTPConcurrentRequestsRespectLimit(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := f.svc.RequestOTP(ctx, "081200000001", auth.RequestMeta{IP: "7.7.7.7"})
			errs <- err
		}()
	}
	okCount := 0
	for i := 0; i < 8; i++ {
		if <-errs == nil {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("%d concurrent requests succeeded; the resend gap must allow exactly one", okCount)
	}
}

func TestOTPWrongCodeFiveTimesLocks(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	phone := "081234567891"
	res := f.request(t, phone, "1.1.1.1")
	wrong := "000000"
	if res.DevCode == wrong {
		wrong = "111111"
	}
	for i := 1; i <= auth.OTPMaxAttempts; i++ {
		_, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: wrong, AcceptTerms: true}, auth.RequestMeta{})
		if i < auth.OTPMaxAttempts {
			wantStatus(t, err, http.StatusBadRequest)
		} else {
			wantStatus(t, err, http.StatusTooManyRequests)
		}
	}
	_, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: res.DevCode, AcceptTerms: true}, auth.RequestMeta{})
	wantStatus(t, err, http.StatusTooManyRequests)
	var attempts int
	_ = f.db.QueryRow(ctx, `SELECT attempts FROM otp_challenges WHERE phone = '+6281234567891'`).Scan(&attempts)
	if attempts != auth.OTPMaxAttempts {
		t.Fatalf("attempts = %d", attempts)
	}
	// A new code works again.
	f.clock.Advance(auth.OTPResendGap)
	res = f.request(t, phone, "1.1.1.1")
	if _, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: res.DevCode, AcceptTerms: true}, auth.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
}

func TestOTPExpires(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	res := f.request(t, "081234567892", "1.1.1.1")
	f.clock.Advance(auth.OTPTTL)
	_, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: "081234567892", Code: res.DevCode, AcceptTerms: true}, auth.RequestMeta{})
	wantStatus(t, err, http.StatusBadRequest)
}

func TestOTPSingleUseAndOnlyLatestCode(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	phone := "081234567893"
	first := f.request(t, phone, "1.1.1.1")
	f.clock.Advance(auth.OTPResendGap)
	second := f.request(t, phone, "1.1.1.1")
	if first.DevCode != second.DevCode {
		_, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: first.DevCode, AcceptTerms: true}, auth.RequestMeta{})
		wantStatus(t, err, http.StatusBadRequest)
	}
	if _, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: second.DevCode, AcceptTerms: true}, auth.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: second.DevCode, AcceptTerms: true}, auth.RequestMeta{})
	wantStatus(t, err, http.StatusBadRequest)
}

func TestOTPHashOnlyInDB(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	res := f.request(t, "081234567894", "1.1.1.1")
	var dump string
	_ = f.db.QueryRow(ctx, `SELECT row_to_json(o)::text FROM otp_challenges o`).Scan(&dump)
	if strings.Contains(dump, res.DevCode) {
		t.Fatalf("plain OTP code stored: %s", dump)
	}
}

func TestDemoProviderFixedCode(t *testing.T) {
	f := newFixture(t, auth.DemoSender{})
	res := f.request(t, "081234567895", "1.1.1.1")
	if res.DevCode != auth.DemoOTPCode {
		t.Fatalf("demo code = %q", res.DevCode)
	}
	if _, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: "081234567895", Code: "123456", AcceptTerms: true}, auth.RequestMeta{}); err != nil {
		t.Fatal(err)
	}
}

type failingSender struct{}

func (failingSender) Name() string                                { return auth.ProviderZenziva }
func (failingSender) Send(context.Context, auth.OTPMessage) error { return errors.New("gateway down") }

func TestSendFailureDoesNotBurnQuota(t *testing.T) {
	f := newFixture(t, failingSender{})
	_, err := f.svc.RequestOTP(ctx, "081234567896", auth.RequestMeta{IP: "1.1.1.1"})
	wantStatus(t, err, http.StatusBadGateway)
	var n int
	_ = f.db.QueryRow(ctx, `SELECT count(*) FROM otp_challenges`).Scan(&n)
	if n != 0 {
		t.Fatalf("failed send left %d challenges", n)
	}
}

func TestRegistrationRecordsConsents(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	phone := "081234567897"
	res := f.request(t, phone, "1.1.1.1")
	_, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: res.DevCode}, auth.RequestMeta{})
	if ue := wantStatus(t, err, http.StatusBadRequest); ue.Code != "consent_required" {
		t.Fatalf("code = %q", ue.Code)
	}
	// The correct code is still usable once consent is given.
	v, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: res.DevCode, AcceptTerms: true}, auth.RequestMeta{IP: "1.1.1.1", UserAgent: "UA"})
	if err != nil {
		t.Fatal(err)
	}
	if !v.IsNew || v.Role != auth.RoleUser || v.Token == "" {
		t.Fatalf("verify = %+v", v)
	}
	cs, err := f.svc.Consents(ctx, v.UserID)
	if err != nil || len(cs) != 2 {
		t.Fatalf("consents = %+v, %v", cs, err)
	}
	got := map[string]string{}
	for _, c := range cs {
		got[c.Doc] = c.Version
	}
	if got["terms"] != "2026-11" || got["privacy"] != "2026-11a" {
		t.Fatalf("consent versions = %v", got)
	}
	if need, _ := f.svc.NeedsConsent(ctx, v.UserID); need {
		t.Fatal("NeedsConsent after registration")
	}
	// Existing user: no consent needed, no new account, no duplicate consents.
	f.clock.Advance(auth.OTPResendGap)
	res = f.request(t, phone, "1.1.1.1")
	v2, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: phone, Code: res.DevCode}, auth.RequestMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if v2.IsNew || v2.UserID != v.UserID {
		t.Fatalf("second login = %+v", v2)
	}
	f.svc.Config.TermsVersion = "2027-01"
	if need, _ := f.svc.NeedsConsent(ctx, v.UserID); !need {
		t.Fatal("new terms version must need consent")
	}
}

func TestBlacklistedUserCannotSignIn(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	v := f.login(t, "081234567898")
	if _, err := f.db.Exec(ctx, `UPDATE users SET status = 'blacklist' WHERE id = $1`, v.UserID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Resolve(ctx, v.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("blacklisted session resolved: %v", err)
	}
	f.clock.Advance(auth.OTPResendGap)
	res := f.request(t, "081234567898", "1.1.1.1")
	_, err := f.svc.VerifyOTP(ctx, auth.VerifyInput{Phone: "081234567898", Code: res.DevCode}, auth.RequestMeta{})
	wantStatus(t, err, http.StatusForbidden)
}

func TestSessionSlidingExpiryAndTouchThrottle(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	v := f.login(t, "081234567899")
	var tokenHash []byte
	_ = f.db.QueryRow(ctx, `SELECT token_hash FROM auth_sessions WHERE id = $1`, v.Session.ID).Scan(&tokenHash)
	if strings.Contains(string(tokenHash), v.Token) || len(tokenHash) != 32 {
		t.Fatal("token must be stored as SHA-256 only")
	}
	lastSeen := func() time.Time {
		var ls time.Time
		_ = f.db.QueryRow(ctx, `SELECT last_seen_at FROM auth_sessions WHERE id = $1`, v.Session.ID).Scan(&ls)
		return ls.UTC()
	}
	f.clock.Advance(time.Minute)
	if _, touched, err := f.svc.Resolve(ctx, v.Token); err != nil || touched {
		t.Fatalf("resolve after 1m: touched=%v err=%v", touched, err)
	}
	if !lastSeen().Equal(t0) {
		t.Fatal("last_seen_at must not be written within 5 minutes")
	}
	f.clock.Set(t0.Add(20 * 24 * time.Hour))
	if _, touched, err := f.svc.Resolve(ctx, v.Token); err != nil || !touched {
		t.Fatalf("resolve after 20d: touched=%v err=%v", touched, err)
	}
	// 40 days after login but only 20 since last activity: still valid (sliding 30 days).
	f.clock.Set(t0.Add(40 * 24 * time.Hour))
	if _, _, err := f.svc.Resolve(ctx, v.Token); err != nil {
		t.Fatalf("sliding session expired early: %v", err)
	}
	f.clock.Advance(auth.SessionTTL + time.Second)
	if _, _, err := f.svc.Resolve(ctx, v.Token); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("expired session resolved: %v", err)
	}
}

func TestSessionRevocation(t *testing.T) {
	f := newFixture(t, auth.EchoSender{})
	a := f.login(t, "081211110000")
	tokB, sessB, err := f.svc.CreateSession(ctx, a.UserID, auth.RequestMeta{UserAgent: "B"})
	if err != nil {
		t.Fatal(err)
	}
	tokC, _, _ := f.svc.CreateSession(ctx, a.UserID, auth.RequestMeta{UserAgent: "C"})
	list, _ := f.svc.Sessions(ctx, a.UserID, a.Session.ID)
	if len(list) != 3 {
		t.Fatalf("sessions = %d", len(list))
	}
	// Another user cannot revoke my session.
	other := f.login(t, "081211110001")
	wantStatus(t, f.svc.RevokeSession(ctx, other.UserID, sessB.ID), http.StatusNotFound)

	if err := f.svc.RevokeSession(ctx, a.UserID, sessB.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Resolve(ctx, tokB); !errors.Is(err, auth.ErrNoSession) {
		t.Fatal("revoked session still valid")
	}
	n, err := f.svc.RevokeOtherSessions(ctx, a.UserID, a.Session.ID)
	if err != nil || n != 1 {
		t.Fatalf("revoke others = %d, %v", n, err)
	}
	if _, _, err := f.svc.Resolve(ctx, tokC); !errors.Is(err, auth.ErrNoSession) {
		t.Fatal("session C still valid")
	}
	if _, _, err := f.svc.Resolve(ctx, a.Token); err != nil {
		t.Fatal("current session must survive")
	}
}

/* ---------------------------------------------------------------- HTTP */

type httpFixture struct {
	*fixture
	srv *httptest.Server
}

func newHTTP(t *testing.T) *httpFixture {
	f := newFixture(t, auth.EchoSender{})
	mux := http.NewServeMux()
	auth.Handlers{S: f.svc}.Register(mux)
	mux.Handle("POST /api/v1/things", auth.RequireUserFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.WriteJSON(w, 200, map[string]any{"ok": true, "user": auth.ViewerFrom(r.Context()).UserID})
	}))
	mux.Handle("GET /api/v1/admin/ping", auth.RequireAdminFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.WriteJSON(w, 200, map[string]any{"ok": true})
	}))
	srv := httptest.NewServer(f.svc.Middleware(mux))
	t.Cleanup(srv.Close)
	return &httpFixture{fixture: f, srv: srv}
}

func (h *httpFixture) do(t *testing.T, method, path, body string, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	return nil
}

func TestHTTPOTPRequestDoesNotLeakRegistration(t *testing.T) {
	h := newHTTP(t)
	h.login(t, "081277770000") // registered
	h.clock.Advance(auth.OTPResendGap)
	_, reg := h.do(t, "POST", "/api/v1/auth/otp/request", `{"phone":"081277770000"}`, nil)
	_, unreg := h.do(t, "POST", "/api/v1/auth/otp/request", `{"phone":"081277770001"}`, nil)
	delete(reg, "phone")
	delete(unreg, "phone")
	delete(reg, "devCode")
	delete(unreg, "devCode")
	rb, _ := json.Marshal(reg)
	ub, _ := json.Marshal(unreg)
	if string(rb) != string(ub) {
		t.Fatalf("responses differ:\n%s\n%s", rb, ub)
	}
}

func TestHTTPCookieFlowAndCSRF(t *testing.T) {
	h := newHTTP(t)
	_, out := h.do(t, "POST", "/api/v1/auth/otp/request", `{"phone":"0812 5555 0000"}`, nil)
	code, _ := out["devCode"].(string)
	resp, out := h.do(t, "POST", "/api/v1/auth/otp/verify", `{"phone":"081255550000","code":"`+code+`","acceptTerms":true}`, nil)
	if resp.StatusCode != 200 || out["isNew"] != true || out["token"] != nil {
		t.Fatalf("verify: %d %v", resp.StatusCode, out)
	}
	c := sessionCookie(resp)
	if c == nil || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("cookie = %+v", c)
	}
	cookie := map[string]string{"Cookie": auth.SessionCookieName + "=" + c.Value}
	withOrigin := func(o string) map[string]string {
		m := map[string]string{"Origin": o}
		for k, v := range cookie {
			m[k] = v
		}
		return m
	}
	// Cookie + mutation without/with wrong Origin → 403.
	if resp, _ := h.do(t, "POST", "/api/v1/things", `{}`, cookie); resp.StatusCode != 403 {
		t.Fatalf("no origin: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, "POST", "/api/v1/things", `{}`, withOrigin("https://evil.example")); resp.StatusCode != 403 {
		t.Fatalf("evil origin: %d", resp.StatusCode)
	}
	if resp, out := h.do(t, "POST", "/api/v1/things", `{}`, withOrigin("https://sewain.example")); resp.StatusCode != 200 || out["user"] == "" {
		t.Fatalf("good origin: %d %v", resp.StatusCode, out)
	}
	ref := map[string]string{"Referer": "https://sewain.example/akun", "Cookie": cookie["Cookie"]}
	if resp, _ := h.do(t, "POST", "/api/v1/things", `{}`, ref); resp.StatusCode != 200 {
		t.Fatalf("referer fallback: %d", resp.StatusCode)
	}
	// Safe methods need no Origin.
	if resp, out := h.do(t, "GET", "/api/v1/auth/sessions", ``, cookie); resp.StatusCode != 200 || len(out["sessions"].([]any)) != 1 {
		t.Fatalf("sessions: %d %v", resp.StatusCode, out)
	}
	// Logout revokes and clears the cookie.
	resp, _ = h.do(t, "POST", "/api/v1/auth/logout", ``, withOrigin("https://sewain.example"))
	if resp.StatusCode != 200 || sessionCookie(resp) == nil || sessionCookie(resp).MaxAge >= 0 {
		t.Fatalf("logout: %d %+v", resp.StatusCode, sessionCookie(resp))
	}
	if resp, _ := h.do(t, "GET", "/api/v1/auth/sessions", ``, cookie); resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}

func TestHTTPBearerFlow(t *testing.T) {
	h := newHTTP(t)
	_, out := h.do(t, "POST", "/api/v1/auth/otp/request", `{"phone":"081266660000"}`, nil)
	resp, out := h.do(t, "POST", "/api/v1/auth/otp/verify", `{"phone":"081266660000","code":"`+out["devCode"].(string)+`","acceptTerms":true,"transport":"bearer"}`, nil)
	tok, _ := out["token"].(string)
	if resp.StatusCode != 200 || tok == "" || sessionCookie(resp) != nil {
		t.Fatalf("bearer verify: %d %v", resp.StatusCode, out)
	}
	bearer := map[string]string{"Authorization": "Bearer " + tok}
	// Bearer mutations are not subject to the Origin check.
	if resp, _ := h.do(t, "POST", "/api/v1/things", `{}`, bearer); resp.StatusCode != 200 {
		t.Fatalf("bearer post: %d", resp.StatusCode)
	}
	// Second session, then revoke all others via DELETE /sessions and one via DELETE /sessions/{id}.
	tok2, s2, _ := h.svc.CreateSession(ctx, out["userId"].(string), auth.RequestMeta{})
	_, list := h.do(t, "GET", "/api/v1/auth/sessions", ``, bearer)
	if n := len(list["sessions"].([]any)); n != 2 {
		t.Fatalf("sessions = %d", n)
	}
	if resp, _ := h.do(t, "DELETE", "/api/v1/auth/sessions/"+s2.ID, ``, bearer); resp.StatusCode != 200 {
		t.Fatalf("delete one: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, "POST", "/api/v1/things", `{}`, map[string]string{"Authorization": "Bearer " + tok2}); resp.StatusCode != 401 {
		t.Fatalf("revoked bearer: %d", resp.StatusCode)
	}
	_, _, _ = h.svc.CreateSession(ctx, out["userId"].(string), auth.RequestMeta{})
	if resp, out := h.do(t, "DELETE", "/api/v1/auth/sessions", ``, bearer); resp.StatusCode != 200 || out["revoked"] != float64(1) {
		t.Fatalf("delete others: %d %v", resp.StatusCode, out)
	}
	// Expired bearer → guest → 401.
	h.clock.Advance(auth.SessionTTL + time.Minute)
	if resp, _ := h.do(t, "POST", "/api/v1/things", `{}`, bearer); resp.StatusCode != 401 {
		t.Fatalf("expired bearer: %d", resp.StatusCode)
	}
}

func TestHTTPBearerLoginDisabled(t *testing.T) {
	h := newHTTP(t)
	h.svc.Config.AllowBearerLogin = false
	_, out := h.do(t, "POST", "/api/v1/auth/otp/request", `{"phone":"081266660001"}`, nil)
	resp, _ := h.do(t, "POST", "/api/v1/auth/otp/verify", `{"phone":"081266660001","code":"`+out["devCode"].(string)+`","acceptTerms":true,"transport":"bearer"}`, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("bearer login must be refused: %d", resp.StatusCode)
	}
}

func TestHTTPRateLimitHeaders(t *testing.T) {
	h := newHTTP(t)
	h.do(t, "POST", "/api/v1/auth/otp/request", `{"phone":"081266660002"}`, nil)
	resp, out := h.do(t, "POST", "/api/v1/auth/otp/request", `{"phone":"081266660002"}`, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || out["retryAfter"] == nil {
		t.Fatalf("429 expected: %d %v %v", resp.StatusCode, resp.Header, out)
	}
}

func TestAdminGrantAndRequireAdmin(t *testing.T) {
	h := newHTTP(t)
	v := h.login(t, "081288880000")
	bearer := map[string]string{"Authorization": "Bearer " + v.Token}
	if resp, _ := h.do(t, "GET", "/api/v1/admin/ping", ``, nil); resp.StatusCode != 401 {
		t.Fatalf("guest admin: %d", resp.StatusCode)
	}
	if resp, out := h.do(t, "GET", "/api/v1/admin/ping", ``, bearer); resp.StatusCode != 403 || out["error"] != auth.MsgAdminOnly {
		t.Fatalf("user admin: %d %v", resp.StatusCode, out)
	}
	if id, err := auth.GrantAdmin(ctx, h.db, "+62 812 8888 0000"); err != nil || id != v.UserID {
		t.Fatalf("grant: %s %v", id, err)
	}
	if resp, _ := h.do(t, "GET", "/api/v1/admin/ping", ``, bearer); resp.StatusCode != 200 {
		t.Fatalf("admin: %d", resp.StatusCode)
	}
	if _, err := auth.RevokeAdmin(ctx, h.db, "081288880000"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.do(t, "GET", "/api/v1/admin/ping", ``, bearer); resp.StatusCode != 403 {
		t.Fatalf("revoked admin: %d", resp.StatusCode)
	}
	if _, err := auth.GrantAdmin(ctx, h.db, "081200000999"); err == nil {
		t.Fatal("grant for unknown phone must fail")
	}
}
