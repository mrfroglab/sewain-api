package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/app/apptest"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

func findUser(d *domain.Data, id string) *domain.User {
	for _, u := range d.Users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func listingInput(photos []string) map[string]any {
	return map[string]any{
		"title": "Tenda dome 2 orang", "category": "camping", "description": "Tenda ringan untuk naik gunung.", "photos": photos,
		"pricePerDay": 35000, "itemValue": 600000, "handover": []string{"cod"},
		"collateralOptions": []map[string]any{{"id": "co-1", "kind": "identitas", "cards": []string{"KTM"}}},
		"location":          map[string]any{"lat": -7.7596, "lng": 110.3727, "areaId": "pogung"}, "radiusKm": 3,
		"rules": map[string]any{"lateFeePerDay": 10000, "lateFeeCapPercent": 100, "cancellation": "Gratis H-1."},
	}
}

// submitKYC uploads KTP + selfie for s and returns the submission id.
func submitKYC(t *testing.T, a *apptest.API, s *apptest.Session, nik, name string) string {
	t.Helper()
	body, ct := apptest.Multipart(map[string]string{"nik": nik, "nameOnKtp": name},
		map[string][]byte{"ktp": apptest.PNG(t, 40), "selfie": apptest.PNG(t, 41)})
	r := a.Do(s, apptest.Req{Method: "POST", Path: "/api/v1/kyc/submissions", Body: body, ContentType: ct})
	if r.Status != 201 {
		t.Fatalf("kyc submit: %d %s", r.Status, r.Raw)
	}
	return r.Body["submission"].(map[string]any)["id"].(string)
}

// TestSignupKYCListingBooking: OTP (echo) sign-up → profile → KYC upload → admin approval (both admin paths) →
// listing + booking, with cookie sessions, the CSRF check and PAYMENT_MODE=offline.
func TestSignupKYCListingBooking(t *testing.T) {
	a := apptest.New(t, apptest.Options{})
	ctx := context.Background()
	lender := a.OTPLogin("0812-1111-0001")
	renter := a.OTPLogin("+6281211110002")
	adm := a.OTPLogin("081211110003")
	if _, err := a.App.Svc.SetRoleByPhone(ctx, "081211110003", "admin", "test"); err != nil {
		t.Fatal(err)
	}

	// Cookie session + CSRF (D6): the browser's Origin must be an allowed one for mutations.
	if r := a.Get(lender, "/api/v1/auth/me"); r.Status != 200 || r.Body["user"].(map[string]any)["phone"] != "081211110001" {
		t.Fatalf("me: %d %s", r.Status, r.Raw)
	}
	upd := map[string]any{"args": []any{lender.UserID, map[string]any{"name": "Lina Lestari"}}}
	if r := a.Do(lender, apptest.Req{Method: "POST", Path: "/api/v1/actions/updateUser", JSON: upd, NoOrigin: true}); r.Status != 403 {
		t.Fatalf("cookie mutation without Origin: %d %s", r.Status, r.Raw)
	}
	if r := a.Do(lender, apptest.Req{Method: "POST", Path: "/api/v1/actions/updateUser", JSON: upd,
		Headers: map[string]string{"Origin": "https://evil.example"}}); r.Status != 403 {
		t.Fatalf("cookie mutation from a foreign Origin: %d", r.Status)
	}
	a.OK(lender, "updateUser", lender.UserID, map[string]any{"name": "Lina Lestari"})
	a.OK(renter, "updateUser", renter.UserID, map[string]any{"name": "Rudi Renter"})
	if r := a.Act(lender, "registerUser", map[string]any{"name": "X", "phone": "081299990000"}); r.Body["ok"] != false {
		t.Fatalf("registerUser outside the demo: %s", r.Raw)
	}

	// Not verified: no listing; the simulated verification is demo-only.
	if r := a.Act(lender, "createListing", lender.UserID, listingInput([]string{"/img/a.webp", "/img/b.webp", "/img/c.webp"})); r.Body["ok"] != false {
		t.Fatalf("listing before KYC: %s", r.Raw)
	}
	if r := a.Act(lender, "submitVerification", lender.UserID, map[string]any{"nik": "3404011201990001", "nameOnKtp": "LINA"}); r.Body["ok"] != false {
		t.Fatalf("simulated verification outside the demo: %s", r.Raw)
	}

	// KYC upload → verification request "menunggu" mirrored in the core.
	submitKYC(t, a, lender, "3404011201990001", "LINA LESTARI")
	st := a.State(lender)
	if u := findUser(st, lender.UserID); u.Verification.Status != "menunggu" {
		t.Fatalf("after upload: %s", u.Verification.Status)
	}
	if len(st.Verifications) != 1 || st.Verifications[0].Status != "menunggu" || st.Verifications[0].Method != "manual" {
		t.Fatalf("verification request: %+v", st.Verifications)
	}
	reqID := st.Verifications[0].ID
	// Admin approves from the core admin panel (reviewVerification → kyc.Decide).
	if r := a.Act(renter, "reviewVerification", renter.UserID, reqID, true); r.Body["ok"] != false {
		t.Fatalf("non-admin review: %s", r.Raw)
	}
	a.OK(adm, "reviewVerification", adm.UserID, reqID, true, nil)
	u := findUser(a.State(lender), lender.UserID)
	if u.Verification.Status != "terverifikasi" || u.Verification.NikMasked == nil || !strings.HasPrefix(*u.Verification.NikMasked, "3404") {
		t.Fatalf("approved: %+v", u.Verification)
	}
	var identities int
	_ = a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM identities WHERE user_id = $1`, lender.UserID).Scan(&identities)
	if identities != 1 {
		t.Fatal("approval must create the identity (one NIK, one account)")
	}
	if r := a.Act(adm, "reviewVerification", adm.UserID, reqID, true); r.Body["ok"] != false {
		t.Fatalf("second decision: %s", r.Raw)
	}

	// Renter: decided through the KYC admin endpoint instead; one NIK, one account.
	if r := a.Do(renter, apptest.Req{Method: "POST", Path: "/api/v1/kyc/submissions", ContentType: "multipart/form-data; boundary=x", Body: strings.NewReader("--x--")}); r.Status == 201 {
		t.Fatal("empty submission accepted")
	}
	body, ct := apptest.Multipart(map[string]string{"nik": "3404011201990001", "nameOnKtp": "RUDI"},
		map[string][]byte{"ktp": apptest.PNG(t, 50), "selfie": apptest.PNG(t, 51)})
	if r := a.Do(renter, apptest.Req{Method: "POST", Path: "/api/v1/kyc/submissions", Body: body, ContentType: ct}); r.Status != 409 {
		t.Fatalf("NIK already used: %d %s", r.Status, r.Raw)
	}
	sub := submitKYC(t, a, renter, "3404015505000002", "RUDI RENTER")
	if r := a.Do(adm, apptest.Req{Method: "POST", Path: "/api/v1/admin/kyc/" + sub + "/decision", JSON: map[string]any{"decision": "approve"}}); r.Status != 200 {
		t.Fatalf("kyc decision: %d %s", r.Status, r.Raw)
	}
	if u := findUser(a.State(renter), renter.UserID); u.Verification.Status != "terverifikasi" {
		t.Fatalf("renter verified: %s", u.Verification.Status)
	}

	// Listing with uploaded photos (GPS stripped, public URL), then a booking.
	var photos []string
	for i := 0; i < 3; i++ {
		r := a.Upload(lender, "referensi", "", apptest.PNG(t, uint8(60+i)))
		if r.Body["ok"] != true {
			t.Fatalf("listing photo: %s", r.Raw)
		}
		src := r.Body["photo"].(map[string]any)["src"].(string)
		if strings.Contains(src, "sig=") {
			t.Fatalf("listing photos are public: %s", src)
		}
		photos = append(photos, src)
	}
	lid := a.OK(lender, "createListing", lender.UserID, listingInput(photos)).(string)
	var listing *domain.Listing
	for _, l := range a.State(renter).Listings {
		if l.ID == lid {
			listing = l
		}
	}
	if listing == nil || listing.Location.Lat == -7.7596 {
		t.Fatalf("listing visible with a public location: %+v", listing)
	}
	bid := a.OK(renter, "createBooking", renter.UserID, map[string]any{"listingId": lid, "startDate": apptest.ISO(2), "endDate": apptest.ISO(3),
		"handover": "cod", "collateralOptionId": "co-1"}).(string)
	a.OK(lender, "acceptBooking", lender.UserID, bid)

	// PAYMENT_MODE=offline: only "langsung", then the provider confirms.
	if r := a.Act(renter, "payBooking", renter.UserID, bid, "qris"); r.Body["ok"] != false {
		t.Fatalf("simulated payment in offline mode: %s", r.Raw)
	}
	a.OK(renter, "payBooking", renter.UserID, bid, "langsung")
	a.OK(lender, "confirmDirectPayment", lender.UserID, bid)
	var b *domain.Booking
	for _, x := range a.State(renter).Bookings {
		if x.ID == bid {
			b = x
		}
	}
	if b == nil || b.Payment.Status != "lunas" || domain.Deref(b.Payment.Method, "") != "langsung" || b.Payment.ConfirmedAt == nil {
		t.Fatalf("payment: %+v", b)
	}

	// Logout clears the cookie and ends the session.
	if r := a.Do(renter, apptest.Req{Method: "POST", Path: "/api/v1/auth/logout"}); r.Status != 200 {
		t.Fatalf("logout: %d", r.Status)
	}
	if r := a.Get(renter, "/api/v1/auth/me"); r.Status != 401 {
		t.Fatalf("after logout: %d", r.Status)
	}
}

// TestBlacklistAddsNIK: setUserStatus(blacklist) puts the verified NIK on nik_blacklist and ends the sessions.
func TestBlacklistAddsNIK(t *testing.T) {
	a := apptest.New(t, apptest.Options{})
	ctx := context.Background()
	user := a.OTPLogin("081222220001")
	adm := a.OTPLogin("081222220002")
	if _, err := a.App.Svc.SetRoleByPhone(ctx, "081222220002", "admin", "test"); err != nil {
		t.Fatal(err)
	}
	sub := submitKYC(t, a, user, "3404012304910003", "BUDI")
	a.Do(adm, apptest.Req{Method: "POST", Path: "/api/v1/admin/kyc/" + sub + "/decision", JSON: map[string]any{"decision": "approve"}})
	a.OK(adm, "setUserStatus", adm.UserID, user.UserID, "blacklist", "Penipuan berulang")
	var n int
	_ = a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM nik_blacklist b JOIN identities i USING (nik_hmac) WHERE i.user_id = $1`, user.UserID).Scan(&n)
	if n != 1 {
		t.Fatal("blacklisted user's NIK must be on nik_blacklist")
	}
	if r := a.Get(user, "/api/v1/auth/me"); r.Status != 401 {
		t.Fatalf("blacklisted session: %d", r.Status)
	}
	// The same NIK cannot come back on a new account.
	other := a.OTPLogin("081222220003")
	body, ct := apptest.Multipart(map[string]string{"nik": "3404012304910003", "nameOnKtp": "BUDI"},
		map[string][]byte{"ktp": apptest.PNG(t, 70), "selfie": apptest.PNG(t, 71)})
	if r := a.Do(other, apptest.Req{Method: "POST", Path: "/api/v1/kyc/submissions", Body: body, ContentType: ct}); r.Status != 403 {
		t.Fatalf("blacklisted NIK: %d %s", r.Status, r.Raw)
	}
}

// TestConfigEndpoint: GET /api/v1/config contract (web-v2 needs #12).
func TestConfigEndpoint(t *testing.T) {
	demo := apptest.New(t, apptest.Options{Demo: true})
	r := demo.Get(nil, "/api/v1/config")
	if r.Status != 200 || r.Header.Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("config: %d %v", r.Status, r.Header)
	}
	want := `{"appEnv":"demo","auth":{"otpProvider":"demo"},"demoMode":true,"legal":{"privacyVersion":"draft","termsVersion":"draft"},"ok":true,"paymentMode":"simulated","push":null}`
	if got, _ := json.Marshal(r.Body); string(got) != want {
		t.Fatalf("config body:\n got %s\nwant %s", got, want)
	}

	pub, priv := vapidKeys(t)
	st := apptest.New(t, apptest.Options{Env: map[string]string{"VAPID_PUBLIC_KEY": pub, "VAPID_PRIVATE_KEY": priv, "VAPID_SUBJECT": "mailto:ops@sewain.test"}})
	r = st.Get(nil, "/api/v1/config")
	if r.Body["appEnv"] != "staging" || r.Body["demoMode"] != false || r.Body["paymentMode"] != "offline" ||
		r.Body["auth"].(map[string]any)["otpProvider"] != "echo" || r.Body["push"].(map[string]any)["vapidPublicKey"] != pub ||
		r.Body["legal"].(map[string]any)["termsVersion"] != "2026-11-test" {
		t.Fatalf("staging config: %s", r.Raw)
	}
}

// TestOTPRateLimit: the auth profile as configured (here RATE_AUTH_PER_MIN=10, RATE_AUTH_BURST=5 per IP) answers 429
// before the OTP gateway is reached.
func TestOTPRateLimit(t *testing.T) {
	a := apptest.New(t, apptest.Options{RealRateLimits: true, Env: map[string]string{"RATE_AUTH_PER_MIN": "10", "RATE_AUTH_BURST": "5"}})
	var last apptest.Resp
	for i := 0; i < 6; i++ {
		last = a.Do(nil, apptest.Req{Method: "POST", Path: "/api/v1/auth/otp/request", JSON: map[string]any{"phone": "08123330000" + string(rune('0'+i))}})
		if i < 5 && last.Status != 200 {
			t.Fatalf("request %d: %d %s", i, last.Status, last.Raw)
		}
	}
	if last.Status != http.StatusTooManyRequests || last.Header.Get("Retry-After") == "" || !strings.HasPrefix(last.Body["error"].(string), "Terlalu banyak permintaan") {
		t.Fatalf("6th OTP request: %d %v %s", last.Status, last.Header, last.Raw)
	}
	// The whole API is not blocked for that IP.
	if r := a.Get(nil, "/api/v1/config"); r.Status != 200 {
		t.Fatalf("other routes: %d", r.Status)
	}
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
