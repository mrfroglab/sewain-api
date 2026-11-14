package domain

import (
	"encoding/json"
	"regexp"
	"sync"
	"time"
)

// UserError is a rule violation shown to the user as-is (Bahasa Indonesia).
type UserError string

func (e UserError) Error() string { return string(e) }

func fail(msg string) error { return UserError(msg) }

// Engine owns the authoritative in-memory dataset. Every mutation runs under one mutex and records which
// rows changed; the caller persists those rows to Postgres (write-through) before releasing the lock.
type Engine struct {
	mu       sync.Mutex
	d        *Data
	dirty    map[string]map[ID]bool
	settings bool
	realNow  func() int64
}

func NewEngine(d *Data) *Engine {
	e := &Engine{d: d, realNow: func() int64 { return time.Now().UnixMilli() }}
	e.normalize()
	e.resetDirty()
	return e
}

func (e *Engine) resetDirty() {
	e.dirty = map[string]map[ID]bool{}
	e.settings = false
}

func (e *Engine) touch(coll string, id ID) {
	if e.dirty[coll] == nil {
		e.dirty[coll] = map[ID]bool{}
	}
	e.dirty[coll][id] = true
}

// Lock/Unlock expose the mutex so the HTTP layer can persist changes atomically with the mutation.
func (e *Engine) Lock()   { e.mu.Lock() }
func (e *Engine) Unlock() { e.mu.Unlock() }

// Now is the demo clock (real time + offset), PRD §13 DEMO_MODE "timer dipercepat".
func (e *Engine) Now() int64 { return e.realNow() + e.d.ClockOffset }

func (e *Engine) Data() *Data { return e.d }

// Replace swaps the whole dataset (demo reset). Everything is considered dirty.
func (e *Engine) Replace(d *Data) {
	e.d = d
	e.normalize()
	e.resetDirty()
}

// normalize guarantees non-nil slices/maps so the JSON never contains null where the web app expects arrays.
func (e *Engine) normalize() {
	d := e.d
	if d.Favorites == nil {
		d.Favorites = map[ID][]ID{}
	}
	if d.ChatReads == nil {
		d.ChatReads = map[string]int64{}
	}
	if d.BannedKeywords == nil {
		d.BannedKeywords = []string{}
	}
	for _, l := range d.Listings {
		normListing(l)
	}
	for _, b := range d.Bookings {
		if b.Agreement.ReferencePhotos == nil {
			b.Agreement.ReferencePhotos = []string{}
		}
	}
	for _, x := range d.Disputes {
		if x.Proposals == nil {
			x.Proposals = []DisputeProposal{}
		}
		if x.PhotoIDs == nil {
			x.PhotoIDs = []ID{}
		}
	}
	for _, w := range d.Wanted {
		if w.Offers == nil {
			w.Offers = []WantedOffer{}
		}
	}
	nonNil(&d.Users)
	nonNil(&d.Verifications)
	nonNil(&d.Listings)
	nonNil(&d.Bookings)
	nonNil(&d.Photos)
	nonNil(&d.Messages)
	nonNil(&d.Reviews)
	nonNil(&d.Reports)
	nonNil(&d.Disputes)
	nonNil(&d.Strikes)
	nonNil(&d.Audit)
	nonNil(&d.Wanted)
	nonNil(&d.Notifications)
}

func nonNil[T any](s *[]T) {
	if *s == nil {
		*s = []T{}
	}
}

func normListing(l *Listing) {
	if l.Photos == nil {
		l.Photos = []string{}
	}
	if l.CollateralOptions == nil {
		l.CollateralOptions = []CollateralOption{}
	}
	if l.Handover == nil {
		l.Handover = []string{}
	}
}

// Snapshot is the dataset as JSON in the web app's DataState shape, plus the server clock for sync.
func (e *Engine) Snapshot() ([]byte, error) {
	type snap struct {
		*Data
		ServerNow int64 `json:"serverNow"`
	}
	return json.Marshal(snap{Data: e.d, ServerNow: e.realNow()})
}

/* ----------------------------------------------------------------- lookups */

func (e *Engine) user(id ID) *User {
	for _, u := range e.d.Users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (e *Engine) listing(id ID) *Listing {
	for _, l := range e.d.Listings {
		if l.ID == id {
			return l
		}
	}
	return nil
}

func (e *Engine) booking(id ID) *Booking {
	for _, b := range e.d.Bookings {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (e *Engine) dispute(id ID) *Dispute {
	for _, d := range e.d.Disputes {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (e *Engine) User(id ID) *User {
	return e.user(id)
}

func (e *Engine) isAdmin(id ID) bool {
	u := e.user(id)
	return u != nil && u.Role == "admin"
}

func (e *Engine) canTransact(id ID) error {
	u := e.user(id)
	if u == nil {
		return fail("Akun tidak ditemukan.")
	}
	if u.Status != "aktif" {
		if u.Status == "suspend" {
			return fail("Akun sedang ditangguhkan.")
		}
		return fail("Akun sedang diblokir.")
	}
	if u.Verification.Status != "terverifikasi" {
		return fail("Verifikasi KTP + selfie dulu sebelum menyewa atau menyewakan.")
	}
	return nil
}

func isParty(b *Booking, actor ID, role string) bool {
	switch role {
	case "renter":
		return b.RenterID == actor
	case "provider":
		return b.ProviderID == actor
	}
	return b.RenterID == actor || b.ProviderID == actor
}

func (e *Engine) titleOf(b *Booking) string {
	if l := e.listing(b.ListingID); l != nil {
		return l.Title
	}
	return "barang"
}

func bookingHref(b *Booking) string { return "/sewaan/" + b.ID }

/* --------------------------------------------------------------- side effects */

func (e *Engine) notify(userID ID, kind, title, body, href string) {
	n := &Notification{ID: NewID("n"), UserID: userID, Kind: kind, Title: title, Body: body, At: e.Now()}
	if href != "" {
		n.Href = ptr(href)
	}
	e.d.Notifications = append([]*Notification{n}, e.d.Notifications...)
	e.touch("notifications", n.ID)
}

func (e *Engine) sysMsg(bookingID ID, text string) {
	m := &Message{ID: NewID("m"), BookingID: bookingID, SenderID: "system", Text: ptr(text), At: e.Now()}
	e.d.Messages = append(e.d.Messages, m)
	e.touch("messages", m.ID)
}

func (e *Engine) audit(actor ID, action, target string, oldV, newV *string) {
	a := &AuditEntry{ID: NewID("a"), ActorID: actor, Action: action, Target: target, OldValue: oldV, NewValue: newV, At: e.Now()}
	e.d.Audit = append([]*AuditEntry{a}, e.d.Audit...)
	e.touch("audit", a.ID)
}

var idPattern = regexp.MustCompile(`^[a-z]{1,4}-[a-z0-9]{6,32}$`)

// pickID accepts the id generated by the client (so optimistic UI and navigation stay consistent)
// when it is well-formed, has the right prefix and is unused; otherwise a fresh id is generated.
func pickID(want, prefix string, exists func(ID) bool) ID {
	if want != "" && idPattern.MatchString(want) && len(want) > len(prefix) && want[:len(prefix)+1] == prefix+"-" && !exists(want) {
		return want
	}
	return NewID(prefix)
}

/* ---------------------------------------------------------------- persistence */

// Changes lists everything modified since the last call (rows to upsert).
type Changes struct {
	Rows     map[string][]any // collection -> entities
	Settings bool
	Full     bool // whole dataset replaced (reset)
}

func (c Changes) Empty() bool { return len(c.Rows) == 0 && !c.Settings && !c.Full }

func (e *Engine) TakeChanges() Changes {
	ch := Changes{Rows: map[string][]any{}, Settings: e.settings}
	pick := func(coll string, items int, get func(i int) (ID, any)) {
		ids := e.dirty[coll]
		if len(ids) == 0 {
			return
		}
		for i := 0; i < items; i++ {
			id, v := get(i)
			if ids[id] {
				ch.Rows[coll] = append(ch.Rows[coll], v)
			}
		}
	}
	d := e.d
	pick("users", len(d.Users), func(i int) (ID, any) { return d.Users[i].ID, d.Users[i] })
	pick("verifications", len(d.Verifications), func(i int) (ID, any) { return d.Verifications[i].ID, d.Verifications[i] })
	pick("listings", len(d.Listings), func(i int) (ID, any) { return d.Listings[i].ID, d.Listings[i] })
	pick("bookings", len(d.Bookings), func(i int) (ID, any) { return d.Bookings[i].ID, d.Bookings[i] })
	pick("photos", len(d.Photos), func(i int) (ID, any) { return d.Photos[i].ID, d.Photos[i] })
	pick("messages", len(d.Messages), func(i int) (ID, any) { return d.Messages[i].ID, d.Messages[i] })
	pick("reviews", len(d.Reviews), func(i int) (ID, any) { return d.Reviews[i].ID, d.Reviews[i] })
	pick("reports", len(d.Reports), func(i int) (ID, any) { return d.Reports[i].ID, d.Reports[i] })
	pick("disputes", len(d.Disputes), func(i int) (ID, any) { return d.Disputes[i].ID, d.Disputes[i] })
	pick("strikes", len(d.Strikes), func(i int) (ID, any) { return d.Strikes[i].ID, d.Strikes[i] })
	pick("audit", len(d.Audit), func(i int) (ID, any) { return d.Audit[i].ID, d.Audit[i] })
	pick("wanted", len(d.Wanted), func(i int) (ID, any) { return d.Wanted[i].ID, d.Wanted[i] })
	pick("notifications", len(d.Notifications), func(i int) (ID, any) { return d.Notifications[i].ID, d.Notifications[i] })
	if len(ch.Rows) > 0 || ch.Settings {
		d.Version++
		ch.Settings = true // version lives in settings
	}
	e.resetDirty()
	return ch
}

/* -------------------------------------------------------------------- clock */

func (e *Engine) AdvanceClock(ms int64) {
	e.d.ClockOffset += ms
	e.settings = true
	e.Tick()
}

func (e *Engine) ResetClock() {
	e.d.ClockOffset = 0
	e.settings = true
}

// Tick runs the time-based transitions (PRD §13 job berkala): EXPIRED after 24h, OVERDUE flag,
// auto-COMPLETED 48h after collateral return, and L1 → L2 escalation after the 48h negotiation window.
func (e *Engine) Tick() {
	t := e.Now()
	for _, b := range e.d.Bookings {
		switch {
		case b.Status == "REQUESTED" && t >= b.ExpiresAt:
			b.Status = "EXPIRED"
			e.touch("bookings", b.ID)
			e.notify(b.RenterID, "booking", "Request kadaluarsa", e.titleOf(b)+": penyedia tidak merespons dalam 24 jam. Coba barang lain di sekitarmu.", bookingHref(b))
			e.notify(b.ProviderID, "booking", "Request kadaluarsa", "Request untuk "+e.titleOf(b)+" kadaluarsa karena tidak direspons 24 jam.", bookingHref(b))
			e.sysMsg(b.ID, "Request kadaluarsa: tidak direspons dalam 24 jam.")
		case (b.Status == "ACTIVE" || b.Status == "DISPUTED") && !b.Overdue && b.ReturnedAt == nil && t > EndOfDay(b.EndDate):
			b.Overdue = true
			e.touch("bookings", b.ID)
			e.notify(b.RenterID, "booking", "Kamu telat mengembalikan barang", e.titleOf(b)+" seharusnya kembali "+b.EndDate+". Denda "+Rupiah(b.Agreement.LateFeePerDay)+"/hari berlaku.", bookingHref(b))
			e.notify(b.ProviderID, "booking", "Booking telat dikembalikan", e.titleOf(b)+" sudah lewat tanggal kembali.", bookingHref(b))
			e.sysMsg(b.ID, "Tanggal kembali sudah lewat. Booking ditandai TELAT. Denda per hari berlaku sesuai kesepakatan.")
		case b.Status == "RETURNED" && b.CollateralReturn != nil && b.CollateralReturn.AutoCompleteAt != nil && b.CollateralReturn.RenterConfirmedAt == nil && t >= *b.CollateralReturn.AutoCompleteAt:
			b.Status = "COMPLETED"
			b.CompletedAt = ptr(t)
			b.CompletedAuto = ptr(true)
			e.touch("bookings", b.ID)
			e.notify(b.RenterID, "booking", "Sewa selesai otomatis", e.titleOf(b)+": tidak ada konfirmasi jaminan dalam 48 jam, status otomatis selesai. Beri rating, yuk.", bookingHref(b))
			e.notify(b.ProviderID, "booking", "Sewa selesai", e.titleOf(b)+" selesai otomatis setelah 48 jam.", bookingHref(b))
			e.sysMsg(b.ID, "Penyewa tidak merespons dalam 48 jam. Status otomatis SELESAI.")
		}
	}
	for _, d := range e.d.Disputes {
		if d.Status == "negosiasi" && t >= d.L1Deadline {
			d.Layer, d.Status = "L2", "mediasi"
			e.touch("disputes", d.ID)
			title := "Booking"
			if b := e.booking(d.BookingID); b != nil {
				title = e.titleOf(b)
				e.sysMsg(b.ID, "Batas negosiasi 48 jam habis. Sengketa otomatis naik ke mediasi admin (L2).")
			}
			e.notify(AdminID, "sengketa", "Sengketa naik ke mediasi", title+": negosiasi 48 jam berakhir tanpa kesepakatan.", "/admin/sengketa/"+d.ID)
		}
	}
}

const AdminID = "u-admin"
