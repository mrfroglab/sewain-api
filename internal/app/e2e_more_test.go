package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mrfroglab/sewain-api/internal/app/apptest"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/geo"
	"github.com/mrfroglab/sewain-api/internal/notify"
	"github.com/mrfroglab/sewain-api/internal/service"
)

func vapidKeys(t *testing.T) (string, string) {
	pub, priv, err := notify.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func get(t *testing.T, url string, header http.Header) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

// TestPhotoAccess: evidence is served with a signed URL (parties: watermarked copy) or a session of a party;
// other users get 403, signatures expire, and the snapshot of a stranger does not list the photo.
func TestPhotoAccess(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	raka, dina, sinta := a.DemoLogin("u-raka"), a.DemoLogin("u-dina"), a.DemoLogin("u-sinta")
	up := a.Upload(dina, "checkout", "b-accepted", apptest.PNG(t, 90))
	if up.Body["ok"] != true {
		t.Fatalf("upload: %s", up.Raw)
	}
	p := up.Body["photo"].(map[string]any)
	id, src := p["id"].(string), p["src"].(string)
	if !strings.Contains(src, "sig=") || !strings.Contains(src, "v=wm") {
		t.Fatalf("party gets a signed watermarked URL: %s", src)
	}
	if r := a.Upload(sinta, "checkout", "b-accepted", apptest.PNG(t, 91)); r.Status != 403 {
		t.Fatalf("stranger uploads to a booking: %d", r.Status)
	}
	if res := get(t, a.Base+src, nil); res.StatusCode != 200 || res.Header.Get("X-Original-SHA256") != p["sha256"] {
		t.Fatalf("signed URL: %d %v", res.StatusCode, res.Header)
	}
	plain := a.Base + "/api/v1/photos/" + id
	if res := get(t, plain, nil); res.StatusCode != 403 {
		t.Fatalf("guest without signature: %d", res.StatusCode)
	}
	if res := get(t, plain, http.Header{"Authorization": {"Bearer " + sinta.Token}}); res.StatusCode != 403 {
		t.Fatalf("other user: %d", res.StatusCode)
	}
	if res := get(t, plain, http.Header{"Authorization": {"Bearer " + raka.Token}}); res.StatusCode != 200 {
		t.Fatalf("party session: %d", res.StatusCode)
	}
	// Tampering the variant away breaks the signature.
	if res := get(t, a.Base+strings.Replace(src, "v=wm", "v=", 1), nil); res.StatusCode != 403 {
		t.Fatalf("tampered URL: %d", res.StatusCode)
	}
	// Snapshot: the renter sees it (signed for him), a stranger does not see it at all.
	found := ""
	for _, ph := range a.State(raka).Photos {
		if ph.ID == id {
			found = ph.Src
		}
	}
	if !strings.Contains(found, "u=u-raka") {
		t.Fatalf("renter snapshot src: %q", found)
	}
	for _, ph := range a.State(sinta).Photos {
		if ph.ID == id {
			t.Fatal("stranger's snapshot lists the photo")
		}
	}
	// Expired signature (URLs live 1 h–1 h 15 min).
	a.Clock.Add(2 * time.Hour)
	res := get(t, a.Base+src, nil)
	if res.StatusCode != 403 {
		t.Fatalf("expired signature: %d", res.StatusCode)
	}
}

type wsClient struct {
	mu     sync.Mutex
	events []map[string]any
}

func (c *wsClient) changed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e["type"] == "changed" {
			n++
		}
	}
	return n
}

func dial(ctx context.Context, t *testing.T, a *apptest.API, token string) *wsClient {
	t.Helper()
	url := strings.Replace(a.Base, "http", "ws", 1) + "/api/v1/events"
	if token != "" {
		url += "?token=" + token
	}
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	c := &wsClient{}
	go func() {
		for {
			_, msg, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(msg, &m)
			c.mu.Lock()
			c.events = append(c.events, m)
			c.mu.Unlock()
		}
	}()
	waitFor(t, "hello", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.events) > 0 })
	return c
}

// TestEventsOnlyToParties: a private change reaches the booking parties (and admins), not other users or guests.
func TestEventsOnlyToParties(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	raka, dina, sinta, adm := a.DemoLogin("u-raka"), a.DemoLogin("u-dina"), a.DemoLogin("u-sinta"), a.DemoLogin("u-admin")
	wr, wd, ws, wa, wg := dial(ctx, t, a, raka.Token), dial(ctx, t, a, dina.Token), dial(ctx, t, a, sinta.Token), dial(ctx, t, a, adm.Token), dial(ctx, t, a, "")
	a.OK(raka, "sendMessage", "u-raka", "b-accepted", map[string]any{"text": "Besok jam 9 ya"})
	waitFor(t, "event to both parties and the admin", func() bool { return wr.changed() == 1 && wd.changed() == 1 && wa.changed() == 1 })
	time.Sleep(300 * time.Millisecond)
	if ws.changed() != 0 || wg.changed() != 0 {
		t.Fatalf("private change leaked: sinta %d, guest %d", ws.changed(), wg.changed())
	}
	// A public change (profile) reaches everyone, guests included.
	a.OK(raka, "updateUser", "u-raka", map[string]any{"bio": "Suka naik gunung"})
	waitFor(t, "public event", func() bool { return ws.changed() == 1 && wg.changed() == 1 })
}

// TestOutboxFilled: every in-app notification queues the configured channels in the same transaction.
func TestOutboxFilled(t *testing.T) {
	pub, priv := vapidKeys(t)
	a := apptest.New(t, apptest.Options{Demo: true, Env: map[string]string{"VAPID_PUBLIC_KEY": pub, "VAPID_PRIVATE_KEY": priv,
		"VAPID_SUBJECT": "mailto:ops@sewain.test", "MAIL_PROVIDER": "log", "MAIL_FROM": "Sewain <noreply@sewain.test>", "PUBLIC_URL": "https://sewain.test"}})
	ctx := context.Background()
	raka, bayu := a.DemoLogin("u-raka"), a.DemoLogin("u-bayu")
	sub := map[string]any{"endpoint": "https://fcm.googleapis.com/fcm/send/dev-bayu", "keys": map[string]any{
		"p256dh": "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM", "auth": "tBHItJI5svbpez7KI4CCXg"}}
	if r := a.Do(bayu, apptest.Req{Method: "POST", Path: "/api/v1/push/subscriptions", JSON: sub}); r.Status != 201 {
		t.Fatalf("subscribe: %d %s", r.Status, r.Raw)
	}
	a.OK(raka, "cancelBooking", "u-raka", "b-requested", "Batal naik gunung")
	var n, webpush int
	err := a.App.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE o.channel = 'webpush') FROM notification_outbox o
		JOIN notifications n ON n.id = o.notification_id WHERE o.user_id = 'u-bayu' AND n.title = 'Booking dibatalkan'`).Scan(&n, &webpush)
	if err != nil || webpush != 1 {
		t.Fatalf("outbox for the cancellation: %d rows, %d webpush (%v)", n, webpush, err)
	}
	// A rejected action leaves nothing behind.
	var before, after int
	_ = a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&before)
	if r := a.Act(raka, "cancelBooking", "u-raka", "b-requested", "lagi"); r.Body["ok"] != false {
		t.Fatalf("second cancel: %s", r.Raw)
	}
	_ = a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&after)
	if after != before {
		t.Fatal("rolled back action must not queue notifications")
	}
}

// TestRetention runs the retention job with a clock moved forward: evidence 90 days after COMPLETED (kept while a
// dispute is open), read notifications after 180 days, sessions, idempotency keys, old outbox rows, and the
// stale-KYC admin alert.
func TestRetention(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	ctx := context.Background()
	raka, dina := a.DemoLogin("u-raka"), a.DemoLogin("u-dina")
	done := a.Upload(raka, "checkin", "b-completed", apptest.PNG(t, 100)).Body["photo"].(map[string]any)["id"].(string)
	held := a.Upload(raka, "masalah", "b-disputed", apptest.PNG(t, 101)).Body["photo"].(map[string]any)["id"].(string)
	a.Do(dina, apptest.Req{Method: "POST", Path: "/api/v1/actions/markThreadRead", JSON: map[string]any{"args": []any{"u-dina", "b-accepted"}},
		Headers: map[string]string{"Idempotency-Key": "k-ret"}})
	var notes []string
	for _, n := range a.State(raka).Notifications {
		notes = append(notes, n.ID)
	}
	a.OK(raka, "markNotificationsRead", "u-raka", notes)
	_, _ = a.App.Pool.Exec(ctx, `INSERT INTO notification_outbox (user_id, notification_id, channel, payload, status, updated_at)
		VALUES ('u-raka', 'n-old', 'webpush', '{}', 'sent', now() - interval '31 days')`)
	key := func(id string) string {
		var k string
		_ = a.App.Pool.QueryRow(ctx, `SELECT object_key FROM photos WHERE id = $1`, id).Scan(&k)
		return k
	}
	doneKey, heldKey := key(done), key(held)

	a.Clock.Add(181 * 24 * time.Hour)
	if err := a.App.RunRetention(ctx); err != nil {
		t.Fatal(err)
	}
	var photos, read, sessions, keys, outbox int
	_ = a.App.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM photos WHERE id = $1), (SELECT count(*) FROM notifications WHERE user_id = 'u-raka' AND read_at IS NOT NULL),
		(SELECT count(*) FROM auth_sessions), (SELECT count(*) FROM idempotency_keys), (SELECT count(*) FROM notification_outbox WHERE notification_id = 'n-old')`,
		done).Scan(&photos, &read, &sessions, &keys, &outbox)
	if photos != 0 || read != 0 || sessions != 0 || keys != 0 || outbox != 0 {
		t.Fatalf("after retention: photo %d, read notifications %d, sessions %d, idempotency keys %d, outbox %d", photos, read, sessions, keys, outbox)
	}
	if ok, _ := a.App.Storage.Exists(ctx, doneKey); ok {
		t.Fatal("evidence object of a completed booking must be deleted")
	}
	if ok, _ := a.App.Storage.Exists(ctx, heldKey); !ok {
		t.Fatal("evidence of an open dispute must be kept")
	}
	var audit int
	_ = a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'retensi.foto'`).Scan(&audit)
	if audit != 1 {
		t.Fatal("photo purge must be audited")
	}
	// Append-only still holds outside the retention job.
	if _, err := a.App.Pool.Exec(ctx, `DELETE FROM photos WHERE id = $1`, held); err == nil {
		t.Fatal("photos must stay append-only")
	}
}

// TestStaleKYCAlert: a submission waiting more than 48 hours alerts the admins once a day.
func TestStaleKYCAlert(t *testing.T) {
	a := apptest.New(t, apptest.Options{})
	ctx := context.Background()
	user := a.OTPLogin("081244440001")
	a.OTPLogin("081244440002")
	if _, err := a.App.Svc.SetRoleByPhone(ctx, "081244440002", "admin", "test"); err != nil {
		t.Fatal(err)
	}
	submitKYC(t, a, user, "3404012304910005", "SITI")
	count := func() int {
		var n int
		_ = a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE title = 'Verifikasi menunggu lebih dari 48 jam'`).Scan(&n)
		return n
	}
	if err := a.App.RunRetention(ctx); err != nil || count() != 0 {
		t.Fatalf("fresh submission: %v %d", err, count())
	}
	a.Clock.Add(49 * time.Hour)
	_ = a.App.RunRetention(ctx)
	_ = a.App.RunRetention(ctx)
	if count() != 1 {
		t.Fatalf("stale alert: %d", count())
	}
}

// TestExportAndDeleteAccount: export holds own data only; deletion anonymises, keeps the bookings, ends sessions and
// erases identity data. Refused while a rental runs.
func TestExportAndDeleteAccount(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	ctx := context.Background()
	raka := a.DemoLogin("u-raka")
	if r := a.Do(raka, apptest.Req{Method: "DELETE", Path: "/api/v1/me"}); r.Status != 409 {
		t.Fatalf("delete with running rentals: %d %s", r.Status, r.Raw)
	}
	// u-maya only has completed rentals.
	maya := a.DemoLogin("u-maya")
	a.OK(maya, "toggleFavorite", "u-maya", "l-headlamp")
	ex := a.Get(maya, "/api/v1/me/export")
	if ex.Status != 200 || !strings.Contains(ex.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export: %d", ex.Status)
	}
	data := ex.Body["export"].(map[string]any)
	if data["profile"].(map[string]any)["phone"] == "" || len(data["bookings"].([]any)) == 0 || len(data["favorites"].([]any)) != 1 {
		t.Fatalf("export content: %s", ex.Raw)
	}
	for _, phone := range []string{"081227340011", "081578904412"} { // Raka, Bayu
		if strings.Contains(string(ex.Raw), phone) {
			t.Fatal("export leaks another user's phone")
		}
	}
	if _, ok := data["counterparts"].(map[string]any)["u-bayu"]; !ok {
		t.Fatal("counterparts by public name")
	}
	if r := a.Do(maya, apptest.Req{Method: "DELETE", Path: "/api/v1/me"}); r.Status != 200 {
		t.Fatalf("delete: %d %s", r.Status, r.Raw)
	}
	if r := a.Get(maya, "/api/v1/auth/me"); r.Status != 401 {
		t.Fatalf("session after delete: %d", r.Status)
	}
	adm := a.DemoLogin("u-admin")
	st := a.State(adm)
	u := findUser(st, "u-maya")
	if u == nil || u.Name != service.DeletedUserName || u.Phone != "" || u.Email != nil {
		t.Fatalf("anonymised profile: %+v", u)
	}
	kept := 0
	for _, b := range st.Bookings {
		if b.RenterID == "u-maya" {
			kept++
		}
	}
	if kept == 0 {
		t.Fatal("bookings stay for the other party")
	}
	var favs int
	_ = a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM favorites WHERE user_id = 'u-maya'`).Scan(&favs)
	if favs != 0 {
		t.Fatal("favorites deleted")
	}
	if r := a.Do(nil, apptest.Req{Method: "POST", Path: "/api/v1/auth/demo-login", JSON: map[string]any{"userId": "u-maya"}}); r.Status != 404 {
		t.Fatalf("deleted account cannot sign in: %d", r.Status)
	}
}

// TestImportListingsCSV: dry run, a real import with a new owner, and a file with bad rows (nothing written).
func TestImportListingsCSV(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	ctx := context.Background()
	adm, raka := a.DemoLogin("u-admin"), a.DemoLogin("u-raka")
	header := "owner_phone,owner_name,early_lender,title,category,price_per_day,item_value,lat,lng,area_id,handover,collateral,photos\n"
	good := header +
		`081578904412,,,Hammock parasut,camping,15000,200000,-7.7596,110.3727,pogung,cod|jemput,identitas:KTM/SIM,/img/a.webp|/img/b.webp|/img/c.webp` + "\n" +
		`0857 1234 5678,Naila Putri,ya,Kebaya brokat hijau,pakaian-acara,"40.000",900000,-7.7652,110.3752,sendowo,cod,deposit:25%,/img/d.webp|/img/e.webp|/img/f.webp` + "\n"
	post := func(s *apptest.Session, csv, query string) apptest.Resp {
		return a.Do(s, apptest.Req{Method: "POST", Path: "/api/v1/admin/listings/import" + query, Body: strings.NewReader(csv), ContentType: "text/csv"})
	}
	if r := post(raka, good, ""); r.Status != 403 {
		t.Fatalf("non-admin import: %d", r.Status)
	}
	count := func() int {
		var n int
		_ = a.App.Pool.QueryRow(ctx, `SELECT count(*) FROM listings`).Scan(&n)
		return n
	}
	before := count()
	dry := post(adm, good, "?dryRun=1")
	if dry.Status != 200 || dry.Body["created"].(float64) != 2 || dry.Body["dryRun"] != true || count() != before {
		t.Fatalf("dry run: %d %s", dry.Status, dry.Raw)
	}
	r := post(adm, good, "")
	if r.Status != 200 || r.Body["created"].(float64) != 2 || count() != before+2 {
		t.Fatalf("import: %d %s", r.Status, r.Raw)
	}
	rows := r.Body["rows"].([]any)
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	if first["ownerId"] != "u-bayu" || first["status"] != "aktif" || second["ownerCreated"] != true || second["status"] != "jeda" {
		t.Fatalf("rows: %s", r.Raw)
	}
	var status string
	var early bool
	_ = a.App.Pool.QueryRow(ctx, `SELECT verification_status, coalesce(early_lender, false) FROM users WHERE phone = '085712345678'`).Scan(&status, &early)
	if status != "belum" || !early {
		t.Fatalf("new owner: %s %v", status, early)
	}

	bad := header +
		`081578904412,,,Hammock,camping,15000,200000,-7.7596,110.3727,pogung,cod,identitas,/img/a.webp|/img/b.webp|/img/c.webp` + "\n" +
		`081578904412,,,Payung besar,tidak-ada,abc,200000,-7.75,110.37,pogung,terbang,emas:1,/img/a.webp` + "\n" +
		`089999999999,,,Barang tanpa pemilik,camping,1000,2000,-7.75,110.37,pogung,cod,identitas,/img/a.webp|/img/b.webp|/img/c.webp` + "\n"
	before = count()
	r = post(adm, bad, "")
	if r.Status != 422 || r.Body["failed"].(float64) != 2 || count() != before {
		t.Fatalf("bad rows: %d %s", r.Status, r.Raw)
	}
	errs := r.Body["rows"].([]any)[1].(map[string]any)["errors"].([]any)
	if len(errs) < 3 {
		t.Fatalf("row 3 should list each problem: %v", errs)
	}
	if r := post(adm, "title\nx\n", ""); r.Status != 400 || r.Body["columns"] == nil {
		t.Fatalf("missing columns: %d %s", r.Status, r.Raw)
	}
}

// TestSnapshotFields covers the web-v2 snapshot additions in scoped mode: scope/viewerId, demo personas for guests,
// publicStats, listingBusy, both parties' chatReads, review visibleAt, the wanted board and owners not active.
func TestSnapshotFields(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	ctx := context.Background()
	raka, dina := a.DemoLogin("u-raka"), a.DemoLogin("u-dina")

	guest := a.State(nil)
	if guest.Scope != "scoped" || guest.ViewerID != nil {
		t.Fatalf("guest scope/viewer: %s %v", guest.Scope, guest.ViewerID)
	}
	ru := findUser(guest, "u-raka")
	if ru == nil || ru.Demo == nil || ru.Phone != "" || ru.PublicStats == nil || ru.Status != "aktif" {
		t.Fatalf("guest sees the demo persona's public profile: %+v", ru)
	}
	raw := string(a.Get(nil, "/api/v1/state").Raw)
	if strings.Contains(raw, "081227340011") || strings.Contains(raw, `"nikMasked"`) {
		t.Fatal("guest snapshot leaks private data")
	}
	for _, w := range guest.Wanted {
		if w.Status != "terbuka" || len(w.Offers) != 0 {
			t.Fatalf("guest wanted %s: %s with %d offers", w.ID, w.Status, len(w.Offers))
		}
	}
	for _, r := range guest.Reviews {
		if r.VisibleAt == nil {
			t.Fatalf("review %s without visibleAt", r.ID)
		}
	}
	// publicStats matches the trust score formula on the same data (admin sees everything).
	adm := a.DemoLogin("u-admin")
	full := a.State(adm)
	bayu := findUser(guest, "u-bayu")
	completed, disputes := 0, 0
	for _, b := range full.Bookings {
		if (b.RenterID == "u-bayu" || b.ProviderID == "u-bayu") && b.Status == "COMPLETED" {
			completed++
		}
	}
	for _, d := range full.Disputes {
		for _, b := range full.Bookings {
			if b.ID == d.BookingID && (b.RenterID == "u-bayu" || b.ProviderID == "u-bayu") {
				disputes++
			}
		}
	}
	fb := findUser(full, "u-bayu")
	if bayu.PublicStats.CompletedRentals != completed+domain.Deref(fb.BaseRatingCount, 0) || bayu.PublicStats.Disputes != disputes ||
		bayu.PublicStats.Strikes != fb.Strikes || bayu.PublicStats.TrustScore <= 0 {
		t.Fatalf("publicStats: %+v (completed %d, disputes %d)", bayu.PublicStats, completed, disputes)
	}

	me := a.State(raka)
	if me.ViewerID == nil || *me.ViewerID != "u-raka" {
		t.Fatal("viewerId")
	}
	busy := me.ListingBusy["l-kamera-mirrorless"] // b-accepted (Raka ← Dina)
	if len(busy) != 1 || busy[0].Count != 1 {
		t.Fatalf("listingBusy: %+v", me.ListingBusy)
	}
	for _, x := range full.Bookings {
		if x.ID == "b-accepted" && (busy[0].Start != x.StartDate || busy[0].End != x.EndDate) {
			t.Fatalf("busy range %+v vs booking %s–%s", busy[0], x.StartDate, x.EndDate)
		}
	}
	if len(me.Wanted) == 0 {
		t.Fatal("wanted")
	}
	// chatReads of the other party.
	a.OK(dina, "markThreadRead", "u-dina", "b-accepted")
	if _, ok := a.State(raka).ChatReads["u-dina:b-accepted"]; !ok {
		t.Fatal("renter sees the provider's read marker")
	}
	if _, ok := a.State(a.DemoLogin("u-sinta")).ChatReads["u-dina:b-accepted"]; ok {
		t.Fatal("read markers of other people's bookings leak")
	}
	// Listings of a suspended owner disappear from public snapshots (even if still "aktif").
	_, _ = a.App.Pool.Exec(ctx, `UPDATE users SET status = 'suspend' WHERE id = 'u-tini'`)
	a.OK(raka, "toggleFavorite", "u-raka", "l-headlamp") // new version
	for _, l := range a.State(nil).Listings {
		if l.OwnerID == "u-tini" {
			t.Fatalf("listing %s of a suspended owner is public", l.ID)
		}
	}
	// Public locations are the geo fuzz of the precise point.
	for _, l := range a.State(nil).Listings {
		for _, f := range full.Listings {
			if f.ID == l.ID {
				if la, ln := geo.FuzzPoint(f.ID, f.Location.Lat, f.Location.Lng); la != l.Location.Lat || ln != l.Location.Lng {
					t.Fatalf("%s: public point is not geo.FuzzPoint", l.ID)
				}
			}
		}
	}
	if full.Scope != "scoped" {
		t.Fatal("admin snapshot is still marked scoped")
	}
	if st := apptest.New(t, apptest.Options{Demo: true, Scope: "full"}).State(nil); st.Scope != "full" || len(st.Bookings) == 0 {
		t.Fatalf("full scope: %s", st.Scope)
	}
}

// TestIdempotencyBodyKey: the body field idempotencyKey works like the header (header wins), a reused key with other
// arguments is 409.
func TestIdempotencyBodyKey(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	raka := a.DemoLogin("u-raka")
	body := func(listing string) map[string]any {
		return map[string]any{"idempotencyKey": "6f1c2c1e-0000-4000-8000-000000000001", "args": []any{"u-raka", listing}}
	}
	r1 := a.Do(raka, apptest.Req{Method: "POST", Path: "/api/v1/actions/toggleFavorite", JSON: body("l-headlamp")})
	r2 := a.Do(raka, apptest.Req{Method: "POST", Path: "/api/v1/actions/toggleFavorite", JSON: body("l-headlamp"),
		Headers: map[string]string{"Idempotency-Key": "6f1c2c1e-0000-4000-8000-000000000001"}})
	if r1.Body["ok"] != true || string(r1.Raw) != string(r2.Raw) || r2.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("body key replay: %s / %s", r1.Raw, r2.Raw)
	}
	if r := a.Do(raka, apptest.Req{Method: "POST", Path: "/api/v1/actions/toggleFavorite", JSON: body("l-tripod")}); r.Status != 409 {
		t.Fatalf("reused key: %d", r.Status)
	}
	favs := a.State(raka).Favorites["u-raka"]
	n := 0
	for _, f := range favs {
		if f == "l-headlamp" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("toggle ran %d times", n)
	}
}
