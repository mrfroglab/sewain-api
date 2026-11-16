package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mrfroglab/sewain-api/internal/app/apptest"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

type api struct {
	t    *testing.T
	base string
	h    *apptest.API
}

func newAPI(t *testing.T, scope string) *api {
	t.Helper()
	h := apptest.New(t, apptest.Options{Demo: true, Scope: scope})
	return &api{t: t, base: h.Base, h: h}
}

func pngBytes(t *testing.T, seed uint8) []byte { return apptest.PNG(t, seed) }

type resp struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

func (a *api) do(method, path, token string, body any, headers ...string) resp {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, header: res.Header, raw: raw}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (a *api) login(id string) string {
	a.t.Helper()
	r := a.do("POST", "/api/v1/auth/demo-login", "", map[string]any{"userId": id})
	tok, _ := r.body["token"].(string)
	if tok == "" {
		a.t.Fatalf("login %s: %s", id, r.raw)
	}
	return tok
}

func (a *api) act(token, name string, args ...any) resp {
	a.t.Helper()
	return a.do("POST", "/api/v1/actions/"+name, token, map[string]any{"args": args})
}

func (a *api) ok(token, name string, args ...any) any {
	a.t.Helper()
	r := a.act(token, name, args...)
	if r.status != 200 || r.body["ok"] != true {
		a.t.Fatalf("%s: %d %s", name, r.status, r.raw)
	}
	return r.body["value"]
}

func (a *api) state(token string) *domain.Data {
	a.t.Helper()
	r := a.do("GET", "/api/v1/state", token, nil)
	if r.status != 200 {
		a.t.Fatalf("state: %d %s", r.status, r.raw)
	}
	var d domain.Data
	if err := json.Unmarshal(r.raw, &d); err != nil {
		a.t.Fatal(err)
	}
	return &d
}

func (a *api) booking(token, id string) *domain.Booking {
	for _, b := range a.state(token).Bookings {
		if b.ID == id {
			return b
		}
	}
	a.t.Fatalf("booking %s not visible", id)
	return nil
}

func (a *api) upload(token, stage, bookingID string, seed uint8) map[string]any {
	a.t.Helper()
	data := pngBytes(a.t, seed)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "x.png")
	_, _ = fw.Write(data)
	_ = mw.WriteField("stage", stage)
	_ = mw.WriteField("bookingId", bookingID)
	_ = mw.WriteField("source", "kamera")
	_ = mw.WriteField("angle", "Depan")
	_ = mw.Close()
	req, _ := http.NewRequest("POST", a.base+"/api/v1/photos", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		OK    bool           `json:"ok"`
		Photo map[string]any `json:"photo"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	sum := sha256.Sum256(data)
	if !out.OK || out.Photo["sha256"] != hex.EncodeToString(sum[:]) {
		a.t.Fatalf("upload: %+v", out)
	}
	return out.Photo
}

func (a *api) uploads(token, stage, bookingID string, n int) []string {
	ids := []string{}
	for i := 0; i < n; i++ {
		ids = append(ids, a.upload(token, stage, bookingID, uint8(10+i)*3)["id"].(string))
	}
	return ids
}

func iso(days int) string { return time.Now().AddDate(0, 0, days).In(domain.WIB).Format("2006-01-02") }

// TestAPIContract mirrors sewain-web/e2e/api.mjs plus the rest of the rental cycle, the dispute flow, admin actions
// and the demo clock, over HTTP against a real database.
func TestAPIContract(t *testing.T) {
	a := newAPI(t, service.ScopeFull)

	// WebSocket change feed (hello + changed events) for Raka (bearer token as ?token=, demo only).
	raka, bayu, admin, sinta := a.login("u-raka"), a.login("u-bayu"), a.login("u-admin"), a.login("u-sinta")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, strings.Replace(a.base, "http", "ws", 1)+"/api/v1/events?token="+raka, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	var mu sync.Mutex
	events := []map[string]any{}
	go func() {
		for {
			_, msg, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(msg, &m)
			mu.Lock()
			events = append(events, m)
			mu.Unlock()
		}
	}()

	if r := a.do("GET", "/api/health", "", nil); r.status != 200 || r.body["ok"] != true || r.body["demoMode"] != true || r.body["clients"] == nil {
		t.Fatalf("health: %s", r.raw)
	}
	if r := a.do("GET", "/api/v1/auth/me", raka, nil); r.status != 200 || r.body["user"].(map[string]any)["phone"] == "" {
		t.Fatalf("me: %s", r.raw)
	}
	if r := a.do("GET", "/api/v1/auth/me", "", nil); r.status != 401 {
		t.Fatalf("me as guest: %d", r.status)
	}
	if r := a.act(raka, "acceptBooking", "u-bayu", "b-requested"); r.status != 403 || r.body["error"] != "Sesi tidak cocok dengan akun ini. Masuk ulang, ya." {
		t.Fatalf("forged actor: %d %s", r.status, r.raw)
	}
	if r := a.act(raka, "nope", "u-raka"); r.status != 404 {
		t.Fatalf("unknown action: %d", r.status)
	}
	if r := a.act(raka, "tick"); r.body["ignored"] != true {
		t.Fatalf("client-only action: %s", r.raw)
	}

	st := a.state("")
	var listing *domain.Listing
	for _, l := range st.Listings {
		if l.ID == "l-kompor-lapangan" {
			listing = l
		}
	}
	created := a.do("POST", "/api/v1/actions/createBooking", raka, map[string]any{"id": "b-testapi12345", "args": []any{"u-raka",
		map[string]any{"listingId": listing.ID, "startDate": iso(1), "endDate": iso(3), "handover": "jemput", "collateralOptionId": listing.CollateralOptions[0].ID}}})
	if created.body["ok"] != true || created.body["value"] != "b-testapi12345" || created.body["version"].(float64) < 1 {
		t.Fatalf("createBooking keeps client id: %s", created.raw)
	}
	dup := a.act(raka, "createBooking", "u-raka", map[string]any{"listingId": "l-laptop", "startDate": iso(-2), "endDate": iso(1), "handover": "cod", "collateralOptionId": "x"})
	if dup.status != 200 || dup.body["ok"] != false || dup.body["error"] != "Tanggal ambil sudah lewat." || dup.body["version"] == nil {
		t.Fatalf("rule violation → ok:false: %s", dup.raw)
	}
	id := "b-testapi12345"
	a.ok(bayu, "acceptBooking", "u-bayu", id)
	b := a.booking(raka, id)
	if b.Status != "ACCEPTED" || b.MeetingPointDetail == nil || len(b.Agreement.Hash) != 64 {
		t.Fatalf("accepted: %+v", b.Status)
	}
	st = a.state(raka)
	sys, notif := false, false
	for _, m := range st.Messages {
		sys = sys || (m.BookingID == id && m.SenderID == "system")
	}
	for _, n := range st.Notifications {
		notif = notif || (n.UserID == "u-raka" && strings.Contains(strings.ToLower(n.Title), "disetujui"))
	}
	if !sys || !notif {
		t.Fatal("system message + notification")
	}

	// Pay, then check-out photos uploaded over HTTP (server-side SHA-256), served with X-Content-SHA256.
	a.ok(raka, "payBooking", "u-raka", id, "qris")
	p := a.upload(bayu, "checkout", id, 1)
	res, err := http.Get(a.base + p["src"].(string)) // signed URL (watermarked copy for a party)
	if err != nil || res.StatusCode != 200 || res.Header.Get("X-Original-SHA256") != p["sha256"] {
		t.Fatalf("photo served: %v %v", err, res.Header)
	}
	res.Body.Close()
	demo := a.do("POST", "/api/v1/photos/demo", bayu, map[string]any{"url": "/img/listings/x.webp", "stage": "jaminan", "bookingId": id})
	if demo.body["ok"] != true {
		t.Fatalf("demo photo: %s", demo.raw)
	}
	coll := demo.body["photo"].(map[string]any)["id"].(string)
	photoIDs := append([]string{p["id"].(string)}, a.uploads(bayu, "checkout", id, 2)...)
	a.ok(bayu, "submitCheckout", "u-bayu", id, map[string]any{"photoIds": photoIDs, "collateralPhotoIds": []string{coll}})
	a.ok(raka, "agreeCheckout", "u-raka", id)
	if s := a.booking(raka, id).Status; s != "ACTIVE" {
		t.Fatalf("ACTIVE after both agree: %s", s)
	}
	a.ok(bayu, "submitCheckin", "u-bayu", id, map[string]any{"photoIds": a.uploads(bayu, "checkin", id, 3), "result": "sesuai"})
	a.ok(bayu, "returnCollateral", "u-bayu", id)
	a.ok(raka, "confirmCollateral", "u-raka", id)
	if s := a.booking(raka, id).Status; s != "COMPLETED" {
		t.Fatalf("COMPLETED: %s", s)
	}
	a.ok(raka, "submitReview", "u-raka", id, 5, "Mantap")
	a.ok(bayu, "submitReview", "u-bayu", id, 5, "Penyewa rapi")
	reviews := 0
	for _, r := range a.state("").Reviews {
		if r.BookingID == id {
			reviews++
		}
	}
	if reviews != 2 {
		t.Fatalf("two-way reviews: %d", reviews)
	}

	// Dispute: L1 → L2 → resolved by an admin.
	did := a.ok(sinta, "openDispute", "u-sinta", "b-active", map[string]any{"caseType": "rusak-ringan", "description": "Resleting macet"}).(string)
	a.ok(bayu, "proposeSolution", "u-bayu", did, map[string]any{"type": "potong-deposit", "amount": 50000})
	a.ok(sinta, "escalateDispute", "u-sinta", did)
	a.ok(admin, "adminRecommend", "u-admin", did, map[string]any{"text": "Potong Rp25.000", "amount": 25000})
	a.ok(admin, "resolveDispute", "u-admin", did, "Deposit dipotong Rp25.000")
	if s := a.booking(sinta, "b-active").Status; s != "RESOLVED" {
		t.Fatalf("resolved: %s", s)
	}

	// Admin-only actions.
	if r := a.act(raka, "updateCommission", "u-raka", map[string]any{"active": true}); r.body["ok"] != false || r.body["error"] != "Hanya admin." {
		t.Fatalf("non-admin commission: %s", r.raw)
	}
	a.ok(admin, "updateCommission", "u-admin", map[string]any{"active": true, "percent": 10})
	if st := a.state(admin); !st.Commission.Active || st.Audit[0].Action != "komisi.ubah" {
		t.Fatal("commission + audit")
	}
	a.ok(admin, "updateCommission", "u-admin", map[string]any{"active": false})

	// Demo clock: +25h → b-requested EXPIRED.
	if r := a.act(bayu, "advanceClock", 25*3600000); r.body["ok"] != true {
		t.Fatalf("advanceClock: %s", r.raw)
	}
	if s := a.booking(admin, "b-requested").Status; s != "EXPIRED" {
		t.Fatalf("EXPIRED: %s", s)
	}
	if r := a.act(bayu, "advanceClock", 0); r.status != 400 {
		t.Fatalf("invalid jump: %d", r.status)
	}
	if r := a.act("", "resetClock"); r.status != 401 {
		t.Fatalf("demo controls need a session: %d", r.status)
	}
	a.ok(bayu, "resetClock")
	a.ok(admin, "resetDemo")
	if s := a.booking(admin, "b-requested").Status; s != "REQUESTED" {
		t.Fatalf("after reset: %s", s)
	}
	// Tokens survive the reset (the web keeps them).
	a.ok(raka, "toggleFavorite", "u-raka", "l-headlamp")

	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	changed := 0
	for _, e := range events {
		if e["type"] == "changed" {
			changed++
		}
	}
	if len(events) == 0 || events[0]["type"] != "hello" || changed < 5 {
		t.Fatalf("websocket events: %d changed of %d", changed, len(events))
	}
}

func TestStateETagAndScope(t *testing.T) {
	a := newAPI(t, service.ScopeScoped)
	raka := a.login("u-raka")
	r := a.do("GET", "/api/v1/state", raka, nil)
	etag := r.header.Get("ETag")
	if etag == "" || r.body["serverNow"] == nil {
		t.Fatalf("ETag + serverNow: %v", r.header)
	}
	if r2 := a.do("GET", "/api/v1/state", raka, nil, "If-None-Match", etag); r2.status != 304 {
		t.Fatalf("If-None-Match: %d", r2.status)
	}
	if r3 := a.do("GET", "/api/v1/state", "", nil, "If-None-Match", etag); r3.status != 200 {
		t.Fatalf("another viewer must not get a 304: %d", r3.status)
	}
	// Another user's private change keeps Raka's ETag: his refetch is a 304 (load test 7.2).
	sinta := a.login("u-sinta")
	a.ok(sinta, "toggleFavorite", "u-sinta", "l-headlamp")
	if r5 := a.do("GET", "/api/v1/state", raka, nil, "If-None-Match", etag); r5.status != 304 {
		t.Fatalf("another user's favorite must keep the ETag: %d", r5.status)
	}
	a.ok(raka, "toggleFavorite", "u-raka", "l-headlamp")
	if r4 := a.do("GET", "/api/v1/state", raka, nil, "If-None-Match", etag); r4.status != 200 {
		t.Fatalf("new version must not be 304: %d", r4.status)
	}
	// Scoped: the guest does not get phone numbers or bookings.
	guest := a.state("")
	if len(guest.Bookings) != 0 || strings.Contains(string(a.do("GET", "/api/v1/state", "", nil).raw), "081227340011") {
		t.Fatal("guest snapshot leaks private data")
	}
	// Bookings are visible to the parties only.
	if len(a.state(raka).Bookings) == 0 {
		t.Fatal("own bookings")
	}
}

func TestIdempotencyKeyHeader(t *testing.T) {
	a := newAPI(t, service.ScopeFull)
	raka := a.login("u-raka")
	body := map[string]any{"args": []any{"u-raka", map[string]any{"listingId": "l-kompor-lapangan", "startDate": iso(2), "endDate": iso(3), "handover": "jemput",
		"collateralOptionId": "co-g"}}}
	st := a.state(raka)
	for _, l := range st.Listings {
		if l.ID == "l-kompor-lapangan" {
			body["args"].([]any)[1].(map[string]any)["collateralOptionId"] = l.CollateralOptions[0].ID
		}
	}
	before := len(st.Bookings)
	r1 := a.do("POST", "/api/v1/actions/createBooking", raka, body, "Idempotency-Key", "k-1")
	r2 := a.do("POST", "/api/v1/actions/createBooking", raka, body, "Idempotency-Key", "k-1")
	if r1.body["ok"] != true || string(r1.raw) != string(r2.raw) || r2.header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay: %s / %s", r1.raw, r2.raw)
	}
	if after := len(a.state(raka).Bookings); after != before+1 {
		t.Fatalf("one booking expected: %d → %d", before, after)
	}
	body["args"].([]any)[1].(map[string]any)["endDate"] = iso(4)
	if r3 := a.do("POST", "/api/v1/actions/createBooking", raka, body, "Idempotency-Key", "k-1"); r3.status != 409 {
		t.Fatalf("reused key: %d %s", r3.status, r3.raw)
	}
}

func TestSearchEndpoint(t *testing.T) {
	a := newAPI(t, service.ScopeScoped)
	r := a.do("GET", "/api/v1/listings/search?lat=-7.7713&lng=110.3776&radiusKm=3", "", nil)
	if r.body["ok"] != true || len(r.body["hits"].([]any)) <= 30 {
		t.Fatalf("search: %s", r.raw)
	}
	r = a.do("GET", "/api/v1/listings/search?lat=-7.7713&lng=110.3776&radiusKm=5&q=tenda&serah=cod&mulai="+iso(3)+"&selesai="+iso(4), "", nil)
	if r.body["ok"] != true {
		t.Fatalf("filters: %s", r.raw)
	}
	if r := a.do("GET", "/api/v1/listings/search?lat=0&lng=0&radiusKm=3", "", nil); r.status != 400 {
		t.Fatalf("validation: %d", r.status)
	}
}
