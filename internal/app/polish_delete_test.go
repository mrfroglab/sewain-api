package app_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/app/apptest"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// TestDeleteAccountBlockedUntilSettled (UU PDP, FR-A*): DELETE /api/v1/me answers 409 while the user has a rental in
// progress or an open dispute, with a message saying which, and succeeds once the rental is cancelled / the dispute
// is resolved.
func TestDeleteAccountBlockedUntilSettled(t *testing.T) {
	a := apptest.New(t, apptest.Options{Demo: true})
	bayu, adm := a.DemoLogin("u-bayu"), a.DemoLogin("u-admin")
	day := func(n int) string { return domain.AddDays(domain.IsoDate(time.Now().UnixMilli()), n) }
	book := func(s *apptest.Session, from int) string {
		t.Helper()
		return a.OK(s, "createBooking", s.UserID, map[string]any{"listingId": "l-kompor-lapangan", "startDate": day(from), "endDate": day(from + 1),
			"handover": "cod", "collateralOptionId": "co-a"}).(string)
	}
	del := func(s *apptest.Session) apptest.Resp {
		return a.Do(s, apptest.Req{Method: "DELETE", Path: "/api/v1/me"})
	}

	// Maya (only completed rentals in the seed) requests a rental: blocked until she cancels it.
	maya := a.DemoLogin("u-maya")
	bm := book(maya, 40)
	if r := del(maya); r.Status != 409 || !strings.Contains(r.Body["error"].(string), "sewaan yang berjalan") {
		t.Fatalf("delete with a requested rental: %d %s", r.Status, r.Raw)
	}
	a.OK(bayu, "acceptBooking", "u-bayu", bm, nil)
	if r := del(maya); r.Status != 409 {
		t.Fatalf("delete with an accepted rental: %d %s", r.Status, r.Raw)
	}
	a.OK(maya, "cancelBooking", "u-maya", bm, "Tidak jadi")
	if r := del(maya); r.Status != 200 {
		t.Fatalf("delete after canceling: %d %s", r.Status, r.Raw)
	}

	// Dimas rents, the item is handed over, he opens a dispute: blocked (dispute message) until the admin resolves it.
	dimas := a.DemoLogin("u-dimas")
	bd := book(dimas, 50)
	a.OK(bayu, "acceptBooking", "u-bayu", bd, nil)
	var photos []string
	for i := range 4 {
		r := a.Upload(bayu, "checkout", bd, apptest.PNG(t, uint8(10+i)))
		photos = append(photos, r.Body["photo"].(map[string]any)["id"].(string))
	}
	a.OK(bayu, "submitCheckout", "u-bayu", bd, map[string]any{"photoIds": photos[:3], "collateralPhotoIds": photos[3:]})
	a.OK(dimas, "agreeCheckout", "u-dimas", bd)
	if r := del(dimas); r.Status != 409 || !strings.Contains(r.Body["error"].(string), "sewaan yang berjalan") {
		t.Fatalf("delete during an active rental: %d %s", r.Status, r.Raw)
	}
	did := a.OK(dimas, "openDispute", "u-dimas", bd, map[string]any{"caseType": "rusak", "description": "Kompor bocor gas"}).(string)
	r := del(dimas)
	if r.Status != 409 || r.Body["error"] != "Masih ada sengketa yang terbuka. Selesaikan dulu sebelum menghapus akun." {
		t.Fatalf("delete with an open dispute: %d %s", r.Status, r.Raw)
	}
	if r := a.Get(dimas, "/api/v1/auth/me"); r.Status != 200 {
		t.Fatalf("a refused delete must keep the session: %d", r.Status)
	}
	a.OK(adm, "resolveDispute", "u-admin", did, "Kompor diganti penyedia, tanpa kompensasi.")
	if r := del(dimas); r.Status != 200 {
		t.Fatalf("delete after the dispute is resolved: %d %s", r.Status, r.Raw)
	}
	if r := a.Get(dimas, "/api/v1/auth/me"); r.Status != 401 {
		t.Fatalf("session after delete: %d", r.Status)
	}
}
