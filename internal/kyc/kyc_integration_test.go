package kyc_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mrfroglab/sewain-api/internal/auth"
	"github.com/mrfroglab/sewain-api/internal/auth/authtest"
	"github.com/mrfroglab/sewain-api/internal/crypto"
	"github.com/mrfroglab/sewain-api/internal/kyc"
	"github.com/mrfroglab/sewain-api/internal/kyc/kyctest"
)

var (
	ctx   = context.Background()
	t0    = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	admin = auth.Viewer{UserID: "u-admin", Role: auth.RoleAdmin, SessionID: "s-admin"}
	key1  = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	key2  = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
)

const (
	nikA = "3404010101900001"
	nikB = "3404014202950002"
)

// Image fixtures: real magic bytes followed by a marker we can grep for.
func jpeg(marker string) kyc.File {
	return kyc.File{Data: append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), []byte(marker+strings.Repeat("x", 64))...)}
}
func png(marker string) kyc.File {
	return kyc.File{Data: append([]byte("\x89PNG\r\n\x1a\n"), []byte(marker+strings.Repeat("y", 64))...)}
}

type fixture struct {
	db      *pgxpool.Pool
	svc     *kyc.Service
	blobs   *kyctest.MemBlobStore
	audit   *kyctest.MemAuditor
	clock   *authtest.Clock
	decided []kyc.Submission
}

func newService(t *testing.T, db *pgxpool.Pool, blobs *kyctest.MemBlobStore, audit *kyctest.MemAuditor, keys string) *kyc.Service {
	t.Helper()
	cfg, err := kyc.ConfigFromEnv(func(k string) string {
		return map[string]string{"APP_ENV": "production", "KYC_KEYS": keys,
			"NIK_PEPPER": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := kyc.NewService(db, blobs, audit, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func newFixture(t *testing.T) *fixture {
	db := authtest.DB(t)
	f := &fixture{db: db, blobs: kyctest.NewMemBlobStore(), audit: &kyctest.MemAuditor{}, clock: authtest.NewClock(t0)}
	f.svc = newService(t, db, f.blobs, f.audit, "v1:"+key1)
	f.svc.Now = f.clock.Now
	f.svc.Hooks.OnDecided = func(_ context.Context, _ pgx.Tx, s kyc.Submission) error {
		f.decided = append(f.decided, s)
		return nil
	}
	for _, u := range []string{"u-admin", "u-a", "u-b", "u-c"} {
		authtest.AddUser(t, db, u, "+62812000"+strings.TrimPrefix(u, "u-"), "user")
	}
	return f
}

func (f *fixture) submit(t *testing.T, user, nik string) kyc.Submission {
	t.Helper()
	s, err := f.svc.Submit(ctx, user, kyc.SubmitInput{NIK: nik, NameOnKTP: "Budi  Santoso", KTP: jpeg("KTPMARK-" + user), Selfie: png("SELFIEMARK-" + user)})
	if err != nil {
		t.Fatalf("submit(%s): %v", user, err)
	}
	return s
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var ue *auth.Error
	if !errors.As(err, &ue) || ue.Status != status {
		t.Fatalf("want status %d, got %v", status, err)
	}
}

func TestSubmitStoresNoPlaintext(t *testing.T) {
	f := newFixture(t)
	s := f.submit(t, "u-a", nikA)
	if s.Status != kyc.StatusPending || s.NIKMasked != "3404********0001" || s.NameOnKTP != "" {
		t.Fatalf("submission = %+v", s)
	}
	needles := []string{nikA, "Budi Santoso", "KTPMARK-u-a", "SELFIEMARK-u-a"}
	// 1. Every column of every table in the schema (what pg_dump would print), as text and as bytea hex.
	rows, err := f.db.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	tables, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	if len(tables) < 6 {
		t.Fatalf("tables = %v", tables)
	}
	for _, tbl := range tables {
		rows, err := f.db.Query(ctx, `SELECT t::text FROM `+pgx.Identifier{tbl}.Sanitize()+` t`)
		if err != nil {
			t.Fatal(err)
		}
		dumps, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, d := range dumps {
			for _, n := range needles {
				if strings.Contains(d, n) || strings.Contains(d, hex.EncodeToString([]byte(n))) {
					t.Errorf("table %s contains plaintext %q", tbl, n)
				}
			}
		}
	}
	// 2. The blob store holds only ciphertext.
	blobs := f.blobs.Snapshot()
	if len(blobs) != 2 {
		t.Fatalf("blobs = %d", len(blobs))
	}
	for k, b := range blobs {
		if strings.Contains(k, nikA) {
			t.Errorf("blob key leaks NIK: %s", k)
		}
		for _, n := range needles {
			if bytes.Contains(b, []byte(n)) {
				t.Errorf("blob %s contains plaintext %q", k, n)
			}
		}
		if bytes.HasPrefix(b, []byte("\xff\xd8")) || bytes.HasPrefix(b, []byte("\x89PNG")) {
			t.Errorf("blob %s is a plain image", k)
		}
	}
	// 3. Admin can still read it back.
	data, ct, err := f.svc.ReadFile(ctx, admin, s.ID, kyc.FileKTP)
	if err != nil || ct != "image/jpeg" || !bytes.Contains(data, []byte("KTPMARK-u-a")) {
		t.Fatalf("read ktp: %q %v", ct, err)
	}
	q, _ := f.svc.Queue(ctx, admin, kyc.StatusPending, 10)
	if len(q) != 1 || q[0].NameOnKTP != "Budi Santoso" {
		t.Fatalf("queue = %+v", q)
	}
}

func TestOneNIKOneAccount(t *testing.T) {
	f := newFixture(t)
	a := f.submit(t, "u-a", nikA)
	// Another account with the same NIK while A is pending.
	_, err := f.svc.Submit(ctx, "u-b", kyc.SubmitInput{NIK: nikA, NameOnKTP: "Orang Lain", KTP: jpeg("k"), Selfie: jpeg("s")})
	wantStatus(t, err, http.StatusConflict)
	// A cannot open a second pending submission.
	_, err = f.svc.Submit(ctx, "u-a", kyc.SubmitInput{NIK: nikB, NameOnKTP: "Budi", KTP: jpeg("k"), Selfie: jpeg("s")})
	wantStatus(t, err, http.StatusConflict)
	if _, err := f.svc.Decide(ctx, admin, a.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	// After approval: still taken, and A is verified.
	_, err = f.svc.Submit(ctx, "u-b", kyc.SubmitInput{NIK: nikA, NameOnKTP: "Orang Lain", KTP: jpeg("k"), Selfie: jpeg("s")})
	wantStatus(t, err, http.StatusConflict)
	_, err = f.svc.Submit(ctx, "u-a", kyc.SubmitInput{NIK: nikA, NameOnKTP: "Budi", KTP: jpeg("k"), Selfie: jpeg("s")})
	wantStatus(t, err, http.StatusConflict)
	var n int
	_ = f.db.QueryRow(ctx, `SELECT count(*) FROM identities`).Scan(&n)
	if n != 1 {
		t.Fatalf("identities = %d", n)
	}
	// A rejected submission frees the NIK for nobody else but lets the owner retry.
	c := f.submit(t, "u-c", nikB)
	if _, err := f.svc.Decide(ctx, admin, c.ID, "reject", "Foto KTP buram."); err != nil {
		t.Fatal(err)
	}
	f.submit(t, "u-c", nikB)
}

func TestConcurrentSameNIK(t *testing.T) {
	f := newFixture(t)
	errs := make(chan error, 3)
	for _, u := range []string{"u-a", "u-b", "u-c"} {
		go func() {
			_, err := f.svc.Submit(ctx, u, kyc.SubmitInput{NIK: nikA, NameOnKTP: "Budi", KTP: jpeg("k"), Selfie: jpeg("s")})
			errs <- err
		}()
	}
	ok := 0
	for range 3 {
		if <-errs == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d concurrent submissions with one NIK succeeded", ok)
	}
	if len(f.blobs.Snapshot()) != 2 {
		t.Fatalf("failed submissions left blobs behind: %d", len(f.blobs.Snapshot()))
	}
}

func TestBlacklistedNIKRejected(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.BlacklistNIK(ctx, admin, nikA, "kabur bawa kamera"); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Submit(ctx, "u-a", kyc.SubmitInput{NIK: nikA, NameOnKTP: "Budi", KTP: jpeg("k"), Selfie: jpeg("s")})
	wantStatus(t, err, http.StatusForbidden)

	// Blacklisting by user while a submission is pending blocks the approval too.
	b := f.submit(t, "u-b", nikB)
	if err := f.svc.BlacklistUser(ctx, admin, "u-b", "akun ganda"); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Decide(ctx, admin, b.ID, "approve", "")
	wantStatus(t, err, http.StatusForbidden)
	var dump string
	_ = f.db.QueryRow(ctx, `SELECT string_agg(t::text, ',') FROM nik_blacklist t`).Scan(&dump)
	if strings.Contains(dump, nikA) || strings.Contains(dump, nikB) {
		t.Fatal("blacklist stores plain NIK")
	}
	if len(f.audit.Find("kyc.blacklist")) != 2 {
		t.Fatal("blacklist not audited")
	}
	wantStatus(t, f.svc.BlacklistUser(ctx, admin, "u-c", "x"), http.StatusNotFound)
}

func TestValidation(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		in     kyc.SubmitInput
		status int
	}{
		{kyc.SubmitInput{NIK: "123", NameOnKTP: "Budi", KTP: jpeg("k"), Selfie: jpeg("s")}, 400},
		{kyc.SubmitInput{NIK: "0004010101900001", NameOnKTP: "Budi", KTP: jpeg("k"), Selfie: jpeg("s")}, 400},
		{kyc.SubmitInput{NIK: nikA, NameOnKTP: " ", KTP: jpeg("k"), Selfie: jpeg("s")}, 400},
		{kyc.SubmitInput{NIK: nikA, NameOnKTP: "Budi", Selfie: jpeg("s")}, 400},
		{kyc.SubmitInput{NIK: nikA, NameOnKTP: "Budi", KTP: kyc.File{Data: []byte("%PDF-1.4 not an image")}, Selfie: jpeg("s")}, 415},
		{kyc.SubmitInput{NIK: nikA, NameOnKTP: "Budi", KTP: kyc.File{Data: append([]byte("\xff\xd8\xff"), make([]byte, kyc.MaxFileBytes)...)}, Selfie: jpeg("s")}, 413},
	}
	for _, c := range cases {
		_, err := f.svc.Submit(ctx, "u-a", c.in)
		wantStatus(t, err, c.status)
	}
	if len(f.blobs.Snapshot()) != 0 {
		t.Fatal("invalid submissions stored blobs")
	}
}

func TestKeyRotation(t *testing.T) {
	f := newFixture(t)
	old := f.submit(t, "u-a", nikA)
	if _, err := f.svc.Decide(ctx, admin, old.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	rotated := newService(t, f.db, f.blobs, f.audit, "v1:"+key1+",v2:"+key2)
	rotated.Now = f.clock.Now
	data, _, err := rotated.ReadFile(ctx, admin, old.ID, kyc.FileSelfie)
	if err != nil || !bytes.Contains(data, []byte("SELFIEMARK-u-a")) {
		t.Fatalf("v1 file unreadable after rotation: %v", err)
	}
	got, err := rotated.Get(ctx, admin, old.ID)
	if err != nil || got.NameOnKTP != "Budi Santoso" {
		t.Fatalf("v1 name unreadable after rotation: %+v %v", got, err)
	}
	newer, err := rotated.Submit(ctx, "u-b", kyc.SubmitInput{NIK: nikB, NameOnKTP: "Sari", KTP: jpeg("k2"), Selfie: jpeg("s2")})
	if err != nil {
		t.Fatal(err)
	}
	var oldEnc, newEnc []byte
	_ = f.db.QueryRow(ctx, `SELECT nik_enc FROM kyc_submissions WHERE id = $1`, old.ID).Scan(&oldEnc)
	_ = f.db.QueryRow(ctx, `SELECT nik_enc FROM kyc_submissions WHERE id = $1`, newer.ID).Scan(&newEnc)
	if id, _ := crypto.KeyID(oldEnc); id != "v1" {
		t.Fatalf("old key id = %s", id)
	}
	if id, _ := crypto.KeyID(newEnc); id != "v2" {
		t.Fatalf("new key id = %s", id)
	}
	// Uniqueness survives rotation: the NIK HMAC does not depend on KYC_KEYS.
	_, err = rotated.Submit(ctx, "u-c", kyc.SubmitInput{NIK: nikA, NameOnKTP: "Xena", KTP: jpeg("k"), Selfie: jpeg("s")})
	wantStatus(t, err, http.StatusConflict)
}

func TestAdminFileAccessIsAudited(t *testing.T) {
	f := newFixture(t)
	s := f.submit(t, "u-a", nikA)
	if _, _, err := f.svc.ReadFile(ctx, admin, s.ID, kyc.FileKTP); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.ReadFile(ctx, admin, s.ID, kyc.FileSelfie); err != nil {
		t.Fatal(err)
	}
	reads := f.audit.Find("kyc.file.read")
	if len(reads) != 2 || reads[0].ActorID != "u-admin" || reads[0].Target != "kyc_submission:"+s.ID+"/ktp" {
		t.Fatalf("audit = %+v", reads)
	}
	// Even a miss is audited.
	_, _, err := f.svc.ReadFile(ctx, admin, "kyc-missing", kyc.FileKTP)
	wantStatus(t, err, http.StatusNotFound)
	if len(f.audit.Find("kyc.file.read")) != 3 {
		t.Fatal("failed lookup not audited")
	}
	// No audit, no access.
	f.audit.Fail = true
	_, _, err = f.svc.ReadFile(ctx, admin, s.ID, kyc.FileKTP)
	wantStatus(t, err, http.StatusServiceUnavailable)
	f.audit.Fail = false
	// Non-admins never get in.
	_, _, err = f.svc.ReadFile(ctx, auth.Viewer{UserID: "u-a", Role: auth.RoleUser}, s.ID, kyc.FileKTP)
	wantStatus(t, err, http.StatusForbidden)
}

func TestDecide(t *testing.T) {
	f := newFixture(t)
	s := f.submit(t, "u-a", nikA)
	_, err := f.svc.Decide(ctx, admin, s.ID, "reject", "  ")
	wantStatus(t, err, http.StatusBadRequest)
	_, err = f.svc.Decide(ctx, admin, s.ID, "maybe", "")
	wantStatus(t, err, http.StatusBadRequest)
	_, err = f.svc.Decide(ctx, auth.Viewer{UserID: "u-b", Role: auth.RoleUser}, s.ID, "approve", "")
	wantStatus(t, err, http.StatusForbidden)
	got, err := f.svc.Decide(ctx, admin, s.ID, "approve", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != kyc.StatusApproved || got.DecidedBy != "u-admin" || got.FilesPurgeAfter == nil ||
		!got.FilesPurgeAfter.Equal(t0.Add(kyc.FileRetention)) {
		t.Fatalf("decided = %+v", got)
	}
	_, err = f.svc.Decide(ctx, admin, s.ID, "reject", "telat")
	wantStatus(t, err, http.StatusConflict)
	if len(f.decided) != 1 || len(f.audit.Find("kyc.approve")) != 1 {
		t.Fatalf("hook=%d audit=%d", len(f.decided), len(f.audit.Find("kyc.approve")))
	}
	st, found, err := f.svc.Status(ctx, "u-a")
	if err != nil || !found || st.Status != kyc.StatusApproved {
		t.Fatalf("status = %+v %v %v", st, found, err)
	}
}

func TestPurgeAfter30Days(t *testing.T) {
	f := newFixture(t)
	a := f.submit(t, "u-a", nikA)
	pending := f.submit(t, "u-b", nikB)
	if _, err := f.svc.Decide(ctx, admin, a.ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.PurgeFiles(ctx, t0.Add(29*24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("purge at 29d = %d, %v", n, err)
	}
	if n, err := f.svc.PurgeFiles(ctx, t0.Add(kyc.FileRetention)); err != nil || n != 1 {
		t.Fatalf("purge at 30d = %d, %v", n, err)
	}
	blobs := f.blobs.Snapshot()
	for k := range blobs {
		if strings.Contains(k, a.ID) {
			t.Fatalf("blob %s survived purge", k)
		}
	}
	if len(blobs) != 2 {
		t.Fatalf("pending submission's files must stay: %d blobs", len(blobs))
	}
	_, _, err := f.svc.ReadFile(ctx, admin, a.ID, kyc.FileKTP)
	wantStatus(t, err, http.StatusGone)
	got, _ := f.svc.Get(ctx, admin, a.ID)
	if got.FilesPurgedAt == nil || got.Status != kyc.StatusApproved || got.NIKMasked != "3404********0001" {
		t.Fatalf("after purge = %+v", got)
	}
	var verified int
	_ = f.db.QueryRow(ctx, `SELECT count(*) FROM identities WHERE user_id = 'u-a'`).Scan(&verified)
	if verified != 1 {
		t.Fatal("identity must survive purge")
	}
	if n, _ := f.svc.PurgeFiles(ctx, t0.Add(400*24*time.Hour)); n != 0 {
		t.Fatalf("pending submission purged: %d (%s)", n, pending.ID)
	}
}

type autoProvider struct{ decision, reason string }

func (p autoProvider) Name() string { return "fake-vendor" }
func (p autoProvider) Verify(ctx context.Context, _ kyc.VerifyRequest) (kyc.VerifyResult, error) {
	if ctx.Err() != nil {
		return kyc.VerifyResult{}, ctx.Err()
	}
	return kyc.VerifyResult{Decision: p.decision, Reason: p.reason}, nil
}

type brokenProvider struct{}

func (brokenProvider) Name() string { return "broken" }
func (brokenProvider) Verify(context.Context, kyc.VerifyRequest) (kyc.VerifyResult, error) {
	return kyc.VerifyResult{}, errors.New("vendor down")
}

func TestProviderContractAndAutoDecision(t *testing.T) {
	sample := kyc.VerifyRequest{SubmissionID: "kyc-1", UserID: "u-1", NIK: nikA, NameOnKTP: "Budi", KTP: jpeg("k"), Selfie: jpeg("s")}
	kyctest.ProviderContract(t, kyc.ManualProvider{}, sample)
	kyctest.ProviderContract(t, autoProvider{decision: kyc.StatusApproved}, sample)

	f := newFixture(t)
	f.svc.Provider = autoProvider{decision: kyc.StatusApproved}
	s := f.submit(t, "u-a", nikA)
	if s.Status != kyc.StatusApproved || s.DecidedBy != "system:fake-vendor" || len(f.decided) != 1 {
		t.Fatalf("auto approve = %+v", s)
	}
	f.svc.Provider = brokenProvider{}
	s = f.submit(t, "u-b", nikB)
	if s.Status != kyc.StatusPending {
		t.Fatalf("provider failure must fall back to manual queue: %+v", s)
	}
}

/* ---------------------------------------------------------------- HTTP */

func TestHTTPHandlers(t *testing.T) {
	f := newFixture(t)
	dir := &authtest.Directory{DB: f.db}
	authSvc, err := auth.NewService(f.db, dir, auth.EchoSender{}, auth.Config{AppEnv: "staging", OTPPepper: bytes.Repeat([]byte{3}, 32),
		TermsVersion: "1", PrivacyVersion: "1", AllowedOrigins: []string{"https://sewain.example"}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	kyc.Handlers{S: f.svc}.Register(mux)
	srv := httptest.NewServer(authSvc.Middleware(mux))
	defer srv.Close()
	userTok, _, _ := authSvc.CreateSession(ctx, "u-a", auth.RequestMeta{})
	if _, err := f.db.Exec(ctx, `UPDATE users SET role = 'admin' WHERE id = 'u-admin'`); err != nil {
		t.Fatal(err)
	}
	adminTok, _, _ := authSvc.CreateSession(ctx, "u-admin", auth.RequestMeta{})

	do := func(method, path, tok string, body io.Reader, ctype string) (*http.Response, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, body)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	form := func(nik string) (io.Reader, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("nik", nik)
		_ = mw.WriteField("nameOnKtp", "Budi Santoso")
		fw, _ := mw.CreateFormFile("ktp", "ktp.jpg")
		_, _ = fw.Write(jpeg("HTTPKTP").Data)
		fw, _ = mw.CreateFormFile("selfie", "selfie.png")
		_, _ = fw.Write(png("HTTPSELFIE").Data)
		_ = mw.Close()
		return &buf, mw.FormDataContentType()
	}

	body, ct := form(nikA)
	if resp, _ := do("POST", "/api/v1/kyc/submissions", "", body, ct); resp.StatusCode != 401 {
		t.Fatalf("guest submit: %d", resp.StatusCode)
	}
	body, ct = form(nikA)
	resp, out := do("POST", "/api/v1/kyc/submissions", userTok, body, ct)
	if resp.StatusCode != 201 || !bytes.Contains(out, []byte(`"nikMasked":"3404********0001"`)) || bytes.Contains(out, []byte("Budi")) {
		t.Fatalf("submit: %d %s", resp.StatusCode, out)
	}
	id := between(string(out), `"id":"`, `"`)
	if resp, out := do("GET", "/api/v1/kyc/status", userTok, nil, ""); resp.StatusCode != 200 || !strings.Contains(string(out), `"status":"pending"`) {
		t.Fatalf("status: %d %s", resp.StatusCode, out)
	}

	// Every admin route rejects a normal user.
	adminRoutes := []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/kyc", ""},
		{"GET", "/api/v1/admin/kyc/" + id, ""},
		{"POST", "/api/v1/admin/kyc/" + id + "/decision", `{"decision":"approve"}`},
		{"GET", "/api/v1/admin/kyc/" + id + "/files/ktp", ""},
		{"GET", "/api/v1/admin/kyc/" + id + "/files/selfie", ""},
		{"POST", "/api/v1/admin/kyc/blacklist", `{"userId":"u-a","reason":"x"}`},
	}
	for _, r := range adminRoutes {
		resp, out := do(r.method, r.path, userTok, strings.NewReader(r.body), "application/json")
		if resp.StatusCode != 403 || !strings.Contains(string(out), auth.MsgAdminOnly) {
			t.Errorf("%s %s as user: %d %s", r.method, r.path, resp.StatusCode, out)
		}
		if resp, _ := do(r.method, r.path, "", strings.NewReader(r.body), "application/json"); resp.StatusCode != 401 {
			t.Errorf("%s %s as guest: %d", r.method, r.path, resp.StatusCode)
		}
	}
	if len(f.audit.Find("kyc.file.read")) != 0 || len(f.decided) != 0 {
		t.Fatal("rejected requests had side effects")
	}

	resp, out = do("GET", "/api/v1/admin/kyc", adminTok, nil, "")
	if resp.StatusCode != 200 || !strings.Contains(string(out), `"nameOnKtp":"Budi Santoso"`) {
		t.Fatalf("queue: %d %s", resp.StatusCode, out)
	}
	resp, out = do("GET", "/api/v1/admin/kyc/"+id+"/files/ktp", adminTok, nil, "")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("Cache-Control") != "no-store, private" ||
		!bytes.Contains(out, []byte("HTTPKTP")) {
		t.Fatalf("file: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := do("GET", "/api/v1/admin/kyc/"+id+"/files/kk", adminTok, nil, ""); resp.StatusCode != 404 {
		t.Fatalf("unknown kind: %d", resp.StatusCode)
	}
	resp, out = do("POST", "/api/v1/admin/kyc/"+id+"/decision", adminTok, strings.NewReader(`{"decision":"reject","reason":"Selfie tidak jelas."}`), "application/json")
	if resp.StatusCode != 200 || !strings.Contains(string(out), `"status":"rejected"`) {
		t.Fatalf("decide: %d %s", resp.StatusCode, out)
	}
	if reads := f.audit.Find("kyc.file.read"); len(reads) != 2 { // ktp + unknown kind
		t.Fatalf("file reads audited = %d", len(reads))
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	return s[:strings.Index(s, b)]
}
