package service_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

func scopedService(t *testing.T, e *env) *service.Service {
	t.Helper()
	return service.New(e.svc.Pool(), service.Options{DemoMode: true, SnapshotScope: service.ScopeScoped, Now: e.clock.Now})
}

// TestSnapshotPrivacy (F1.6): user A never receives B's phone, e-mail, bookings, messages, evidence photos,
// notifications or verifications; guests receive no private data; public coordinates are not the precise ones.
func TestSnapshotPrivacy(t *testing.T) {
	e := newEnv(t)
	full := e.state()
	scoped := scopedService(t, e)

	users := map[string]*domain.User{}
	for _, u := range full.Users {
		users[u.ID] = u
	}
	listingOwner := map[string]string{}
	precise := map[string][2]float64{}
	for _, l := range full.Listings {
		listingOwner[l.ID] = l.OwnerID
		precise[l.ID] = [2]float64{l.Location.Lat, l.Location.Lng}
	}

	check := func(name string, viewer *service.Viewer) *domain.Data {
		t.Helper()
		snap, err := scoped.Snapshot(e.ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		d := snap.Data
		me := ""
		if viewer != nil {
			me = viewer.UserID
		}
		raw, _ := json.Marshal(snap)
		for _, u := range full.Users {
			if u.ID == me {
				continue
			}
			if strings.Contains(string(raw), `"`+u.Phone+`"`) {
				t.Errorf("%s: phone of %s leaked", name, u.ID)
			}
			if u.Email != nil && strings.Contains(string(raw), *u.Email) {
				t.Errorf("%s: e-mail of %s leaked", name, u.ID)
			}
			if u.Verification.NikMasked != nil && len(*u.Verification.NikMasked) >= 8 && strings.Contains(string(raw), *u.Verification.NikMasked) {
				t.Errorf("%s: NIK of %s leaked", name, u.ID)
			}
		}
		party := map[string]bool{}
		for _, b := range d.Bookings {
			if b.RenterID != me && b.ProviderID != me {
				t.Errorf("%s: foreign booking %s", name, b.ID)
			}
			party[b.ID] = true
		}
		for _, m := range d.Messages {
			if !party[m.BookingID] {
				t.Errorf("%s: foreign message %s", name, m.ID)
			}
		}
		for _, p := range d.Photos {
			if p.UploaderID != me && (p.BookingID == nil || !party[*p.BookingID]) {
				t.Errorf("%s: foreign photo %s", name, p.ID)
			}
		}
		for _, x := range d.Disputes {
			if !party[x.BookingID] {
				t.Errorf("%s: foreign dispute %s", name, x.ID)
			}
		}
		for _, n := range d.Notifications {
			if n.UserID != me {
				t.Errorf("%s: foreign notification %s", name, n.ID)
			}
		}
		for _, v := range d.Verifications {
			if v.UserID != me {
				t.Errorf("%s: foreign verification %s", name, v.ID)
			}
		}
		for _, r := range d.Reports {
			if r.ReporterID != me {
				t.Errorf("%s: foreign report %s", name, r.ID)
			}
		}
		for _, s := range d.Strikes {
			if s.UserID != me {
				t.Errorf("%s: foreign strike %s", name, s.ID)
			}
		}
		for uid := range d.Favorites {
			if uid != me {
				t.Errorf("%s: favorites of %s", name, uid)
			}
		}
		for k := range d.ChatReads {
			if !strings.HasPrefix(k, me+":") || me == "" {
				t.Errorf("%s: chat read %s", name, k)
			}
		}
		if len(d.Audit) > 0 {
			t.Errorf("%s: audit log visible", name)
		}
		for _, u := range d.Users {
			if u.ID != me && (u.Strikes != 0 || u.Phone != "" || u.Verification.NikMasked != nil) {
				t.Errorf("%s: private profile fields of %s", name, u.ID)
			}
		}
		for _, l := range d.Listings {
			p := precise[l.ID]
			if l.OwnerID == me {
				if l.Location.Lat != p[0] || l.Location.Lng != p[1] {
					t.Errorf("%s: owner must see the precise point of %s", name, l.ID)
				}
				continue
			}
			if l.Location.Lat == p[0] || l.Location.Lng == p[1] {
				t.Errorf("%s: precise location of %s leaked", name, l.ID)
			}
			if dist := domain.DistanceMeters(p[0], p[1], l.Location.Lat, l.Location.Lng); dist > 360 {
				t.Errorf("%s: public point of %s is %.0f m away", name, l.ID, dist)
			}
			if l.Status != "aktif" && !partyOfListing(d, l.ID) {
				t.Errorf("%s: inactive foreign listing %s", name, l.ID)
			}
		}
		for _, w := range d.Wanted {
			for _, o := range w.Offers {
				if w.RequesterID != me && o.ProviderID != me {
					t.Errorf("%s: foreign wanted offer %s", name, o.ID)
				}
			}
		}
		return d
	}

	guest := check("guest", nil)
	if len(guest.Bookings)+len(guest.Messages)+len(guest.Photos)+len(guest.Notifications)+len(guest.Verifications)+len(guest.Disputes) != 0 {
		t.Fatal("guest must not see private data")
	}
	if len(guest.Listings) == 0 || len(guest.Users) != len(full.Users) || guest.Commission.Mode == "" {
		t.Fatal("guest must still see listings, public profiles and configuration")
	}

	raka := check("u-raka", &service.Viewer{UserID: "u-raka", Role: "user"})
	var me *domain.User
	for _, u := range raka.Users {
		if u.ID == "u-raka" {
			me = u
		}
	}
	if me == nil || me.Phone != users["u-raka"].Phone || me.Email == nil {
		t.Fatal("own profile must be complete")
	}
	if len(raka.Bookings) == 0 || len(raka.Notifications) == 0 || len(raka.Favorites["u-raka"]) != 2 {
		t.Fatalf("own data missing: %d bookings, %d notifications", len(raka.Bookings), len(raka.Notifications))
	}

	bayu := check("u-bayu", &service.Viewer{UserID: "u-bayu", Role: "user"})
	if len(bayu.Bookings) == 0 {
		t.Fatal("provider must see own bookings")
	}

	admin, err := scoped.Snapshot(e.ctx, &service.Viewer{UserID: "u-admin", Role: "admin"})
	if err != nil || len(admin.Bookings) != len(full.Bookings) || len(admin.Audit) == 0 {
		t.Fatal("admin sees everything")
	}
}

func partyOfListing(d *domain.Data, listingID string) bool {
	for _, b := range d.Bookings {
		if b.ListingID == listingID {
			return true
		}
	}
	return false
}

// TestReviewVisibility (FR-T2): a review is hidden from others until both parties reviewed or 7 days passed;
// the author always sees it.
func TestReviewVisibility(t *testing.T) {
	e := newEnv(t)
	scoped := scopedService(t, e)
	l := e.listing("l-kompor-lapangan")
	id := e.must("u-raka", "createBooking", "u-raka", bookingInput(l, e.today(1), e.today(2))).(string)
	e.must("u-bayu", "acceptBooking", "u-bayu", id)
	e.must("u-bayu", "submitCheckout", "u-bayu", id, map[string]any{"photoIds": e.photos(3, "checkout", "u-bayu", &id), "collateralPhotoIds": e.photos(1, "jaminan", "u-bayu", &id)})
	e.must("u-raka", "agreeCheckout", "u-raka", id)
	e.must("u-bayu", "submitCheckin", "u-bayu", id, map[string]any{"photoIds": e.photos(3, "checkin", "u-bayu", &id), "result": "sesuai"})
	e.must("u-bayu", "returnCollateral", "u-bayu", id)
	e.must("u-raka", "confirmCollateral", "u-raka", id)
	e.must("u-raka", "submitReview", "u-raka", id, 4, "Oke")

	sees := func(viewer *service.Viewer, from string) bool {
		snap, err := scoped.Snapshot(e.ctx, viewer)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range snap.Reviews {
			if r.BookingID == id && r.FromID == from {
				return true
			}
		}
		return false
	}
	raka, bayu := &service.Viewer{UserID: "u-raka", Role: "user"}, &service.Viewer{UserID: "u-bayu", Role: "user"}
	if !sees(raka, "u-raka") || sees(bayu, "u-raka") || sees(nil, "u-raka") {
		t.Fatal("one-sided review: only the author sees it")
	}
	e.must("u-bayu", "submitReview", "u-bayu", id, 5, "Penyewa baik")
	if !sees(bayu, "u-raka") || !sees(nil, "u-bayu") {
		t.Fatal("both reviewed: both reviews are public")
	}

	// 7 days: the job releases a one-sided review.
	e2 := newEnv(t)
	s2 := scopedService(t, e2)
	e2.must("u-bayu", "submitReview", "u-bayu", "b-resolved", 5, "Lancar")
	visible := func() bool {
		snap, _ := s2.Snapshot(e2.ctx, nil)
		for _, r := range snap.Reviews {
			if r.BookingID == "b-resolved" && r.FromID == "u-bayu" {
				return true
			}
		}
		return false
	}
	if visible() {
		t.Fatal("hidden before 7 days")
	}
	if _, err := e2.svc.AdvanceClock(e2.ctx, 7*domain.Day+domain.Hour); err != nil {
		t.Fatal(err)
	}
	if !visible() {
		t.Fatal("released after 7 days")
	}
}

func TestETag(t *testing.T) {
	e := newEnv(t)
	raka := &service.Viewer{UserID: "u-raka", Role: "user"}
	a, _, err := e.svc.ETag(e.ctx, raka)
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := e.svc.ETag(e.ctx, &service.Viewer{UserID: "u-bayu", Role: "user"})
	if a == b {
		t.Fatal("ETag must differ per viewer")
	}
	snap, _ := e.svc.Snapshot(e.ctx, raka)
	if e.svc.ETagFor(raka, snap.Version, snap.ClockOffset) != a {
		t.Fatal("ETag of the snapshot must match")
	}
	e.must("u-raka", "toggleFavorite", "u-raka", "l-headlamp")
	if c, _, _ := e.svc.ETag(e.ctx, raka); c == a {
		t.Fatal("ETag must change with the version")
	}
}
