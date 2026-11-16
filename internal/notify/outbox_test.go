package notify

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

var wib = time.FixedZone("WIB", 7*3600)

func at(h, m int) time.Time { return time.Date(2026, 11, 20, h, m, 0, 0, wib) }

func TestQuietHours(t *testing.T) {
	q := DefaultPrefs().QuietHours
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{at(23, 0), time.Date(2026, 11, 21, 6, 0, 0, 0, wib)},
		{at(22, 0), time.Date(2026, 11, 21, 6, 0, 0, 0, wib)},
		{at(5, 59), at(6, 0)},
		{at(0, 30), at(6, 0)},
		{at(6, 0), time.Time{}},
		{at(12, 0), time.Time{}},
		{at(21, 59), time.Time{}},
		{at(23, 0).UTC(), time.Date(2026, 11, 21, 6, 0, 0, 0, wib)}, // input zone does not matter
	}
	for _, c := range cases {
		got := q.QuietUntil(c.now)
		if !got.Equal(c.want) {
			t.Errorf("QuietUntil(%v) = %v, want %v", c.now, got, c.want)
		}
	}
	q.Enabled = false
	if !q.QuietUntil(at(23, 0)).IsZero() {
		t.Error("disabled quiet hours still deferred")
	}
	day := QuietHours{Enabled: true, Start: "13:00", End: "15:00", Timezone: "Asia/Jakarta"}
	if got := day.QuietUntil(at(14, 0)); !got.Equal(at(15, 0)) {
		t.Errorf("non-wrapping window: %v", got)
	}
	if !day.QuietUntil(at(15, 0)).IsZero() || !day.QuietUntil(at(12, 59)).IsZero() {
		t.Error("non-wrapping window edges")
	}
}

func TestCategoryOf(t *testing.T) {
	for kind, want := range map[string]Category{
		"booking": CategoryBooking, "papan": CategoryBooking, "chat": CategoryChat, "sengketa": CategorySengketa,
		"sistem": CategorySistem, "verifikasi": CategorySistem, "otp": CategorySistem, "baru": CategorySistem,
	} {
		if got := CategoryOf(kind); got != want {
			t.Errorf("CategoryOf(%q) = %s, want %s", kind, got, want)
		}
	}
}

func TestDefaultPrefs(t *testing.T) {
	p := DefaultPrefs()
	for _, c := range Categories {
		if !p.Enabled(ChannelInApp, c) || !p.Enabled(ChannelWebPush, c) || p.Enabled(ChannelWhatsApp, c) {
			t.Errorf("category %s: in-app/push/whatsapp defaults wrong", c)
		}
		wantEmail := c == CategorySengketa || c == CategorySistem
		if p.Enabled(ChannelEmail, c) != wantEmail {
			t.Errorf("email %s = %v, want %v", c, p.Enabled(ChannelEmail, c), wantEmail)
		}
	}
}

func TestPrefsStore(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	addUser(t, db, "u-a", "")
	p, err := LoadPrefs(ctx, db, "u-a")
	if err != nil {
		t.Fatal(err)
	}
	if p.Enabled(ChannelEmail, CategoryBooking) || p.QuietHours.Start != "22:00" {
		t.Fatalf("defaults not returned: %+v", p)
	}
	on, off := true, false
	start := "23:30"
	patch := PrefsPatch{Channels: map[Channel]map[Category]*bool{
		ChannelEmail:   {CategoryBooking: &on},
		ChannelWebPush: {CategoryChat: &off},
		ChannelInApp:   {CategoryChat: &off}, // ignored
	}}
	patch.QuietHours = &struct {
		Enabled  *bool   `json:"enabled"`
		Start    *string `json:"start"`
		End      *string `json:"end"`
		Timezone *string `json:"timezone"`
	}{Start: &start}
	if err := patch.Apply(&p); err != nil {
		t.Fatal(err)
	}
	if err := SavePrefs(ctx, db, "u-a", p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPrefs(ctx, db, "u-a")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled(ChannelEmail, CategoryBooking) || got.Enabled(ChannelWebPush, CategoryChat) ||
		!got.Enabled(ChannelInApp, CategoryChat) || got.QuietHours.Start != "23:30" || got.QuietHours.End != "06:00" {
		t.Fatalf("round trip: %+v", got)
	}

	bad := "25:00"
	patch = PrefsPatch{}
	patch.QuietHours = &struct {
		Enabled  *bool   `json:"enabled"`
		Start    *string `json:"start"`
		End      *string `json:"end"`
		Timezone *string `json:"timezone"`
	}{End: &bad}
	if err := patch.Apply(&got); err != errBadClock {
		t.Fatalf("bad clock: %v", err)
	}
	if err := (PrefsPatch{Channels: map[Channel]map[Category]*bool{"sms": {CategoryChat: &on}}}).Apply(&got); err != errUnknownPref {
		t.Fatalf("unknown channel: %v", err)
	}
}

type outboxRow struct {
	Channel  Channel
	Status   string
	Due      time.Time
	Attempts int
	Payload  Payload
	LastErr  *string
}

func outboxRows(t *testing.T, db DB, notificationID string) map[Channel]outboxRow {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT channel, status, next_attempt_at, attempts, payload, last_error
		FROM notification_outbox WHERE notification_id = $1`, notificationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[Channel]outboxRow{}
	for rows.Next() {
		var r outboxRow
		var payload []byte
		if err := rows.Scan(&r.Channel, &r.Status, &r.Due, &r.Attempts, &payload, &r.LastErr); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(payload, &r.Payload)
		out[r.Channel] = r
	}
	return out
}

const testP256dh = "BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQtUbVlUls0VJXg7A8u-Ts1XbjhazAkj7I99e8QcYP7DkM"
const testAuth = "tBHItJI5svbpez7KI4CCXg"

func TestEnqueueChoosesChannels(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	addUser(t, db, "u-push-mail", "a@contoh.id")
	addUser(t, db, "u-mail-only", "b@contoh.id")
	addUser(t, db, "u-nothing", "")
	sub := SubscriptionInput{Endpoint: "https://fcm.googleapis.com/fcm/send/abc"}
	sub.Keys.P256dh, sub.Keys.Auth = testP256dh, testAuth
	if err := SaveSubscription(ctx, db, "u-push-mail", sub, "test"); err != nil {
		t.Fatal(err)
	}
	noon := at(12, 0)
	e := &Enqueuer{Channels: map[Channel]bool{ChannelWebPush: true, ChannelEmail: true, ChannelWhatsApp: true},
		PublicURL: "https://sewain.test/", Now: func() time.Time { return noon }}

	run := func(n Notification) []Channel {
		t.Helper()
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		got, err := e.Enqueue(ctx, tx, n)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return got
	}
	check := func(name string, got []Channel, want ...Channel) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s: queued %v, want %v", name, got, want)
		}
	}

	check("booking", run(Notification{ID: "n-1", UserID: "u-push-mail", Kind: "booking", Title: "Request sewa baru", Body: "Ada request", Href: "/sewaan/b-1"}),
		ChannelWebPush) // email is off for booking by default, WhatsApp off
	check("sengketa", run(Notification{ID: "n-2", UserID: "u-push-mail", Kind: "sengketa", Title: "Ada laporan masalah"}),
		ChannelWebPush, ChannelEmail)
	check("no subscription", run(Notification{ID: "n-3", UserID: "u-mail-only", Kind: "sistem", Title: "Laporanmu sudah ditinjau"}),
		ChannelEmail)
	check("unreachable", run(Notification{ID: "n-4", UserID: "u-nothing", Kind: "sengketa", Title: "x"}))
	check("idempotent", run(Notification{ID: "n-2", UserID: "u-push-mail", Kind: "sengketa", Title: "Ada laporan masalah"}))

	rows := outboxRows(t, db, "n-1")
	p := rows[ChannelWebPush].Payload
	if p.URL != "https://sewain.test/sewaan/b-1" || p.Href != "/sewaan/b-1" || p.Tag != "n-1" || p.Category != CategoryBooking {
		t.Errorf("payload = %+v", p)
	}
	if rows[ChannelWebPush].Status != "pending" || !rows[ChannelWebPush].Due.Equal(noon) {
		t.Errorf("row = %+v", rows[ChannelWebPush])
	}
	if to := outboxRows(t, db, "n-2")[ChannelEmail].Payload.To; to != "a@contoh.id" {
		t.Errorf("email snapshot = %q", to)
	}

	// Server without email configured never queues email.
	e.Channels = map[Channel]bool{ChannelWebPush: true}
	check("email not configured", run(Notification{ID: "n-5", UserID: "u-mail-only", Kind: "sistem", Title: "x"}))

	// Preferences off → not queued.
	off := false
	prefs := DefaultPrefs()
	if err := (PrefsPatch{Channels: map[Channel]map[Category]*bool{ChannelWebPush: {CategoryBooking: &off}}}).Apply(&prefs); err != nil {
		t.Fatal(err)
	}
	if err := SavePrefs(ctx, db, "u-push-mail", prefs); err != nil {
		t.Fatal(err)
	}
	check("push off for booking", run(Notification{ID: "n-6", UserID: "u-push-mail", Kind: "booking", Title: "x"}))

	// Rolled back with the core's transaction → nothing queued.
	tx, _ := db.Begin(ctx)
	if _, err := e.Enqueue(ctx, tx, Notification{ID: "n-7", UserID: "u-push-mail", Kind: "sistem", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if len(outboxRows(t, db, "n-7")) != 0 {
		t.Error("rolled back notification left outbox rows")
	}
}

func TestEnqueueQuietHours(t *testing.T) {
	db := testPool(t)
	ctx := context.Background()
	addUser(t, db, "u-a", "a@contoh.id")
	night := at(23, 15)
	e := &Enqueuer{Channels: map[Channel]bool{ChannelEmail: true}, PublicURL: "https://sewain.test", Now: func() time.Time { return night }}
	tx, _ := db.Begin(ctx)
	defer tx.Rollback(ctx)
	for _, n := range []Notification{
		{ID: "n-sys", UserID: "u-a", Kind: "sistem", Title: "Laporan ditinjau"},
		{ID: "n-dispute", UserID: "u-a", Kind: "sengketa", Title: "Sengketa naik ke mediasi"},
		{ID: "n-otp", UserID: "u-a", Kind: "sistem", Title: "Kode masuk", Important: true},
	} {
		if _, err := e.Enqueue(ctx, tx, n); err != nil {
			t.Fatal(err)
		}
	}
	morning := time.Date(2026, 11, 21, 6, 0, 0, 0, wib)
	if r := outboxRows(t, tx, "n-sys")[ChannelEmail]; !r.Due.Equal(morning) {
		t.Errorf("sistem at night due %v, want %v", r.Due, morning)
	}
	for _, id := range []string{"n-dispute", "n-otp"} {
		if r := outboxRows(t, tx, id)[ChannelEmail]; !r.Due.Equal(night) || !r.Payload.Important {
			t.Errorf("%s: due %v important %v, want now and important", id, r.Due, r.Payload.Important)
		}
	}
}
