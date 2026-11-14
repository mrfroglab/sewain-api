package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/seed"
)

func newEngine(t *testing.T) *domain.Engine {
	t.Helper()
	d, err := seed.Build(time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	return domain.NewEngine(d)
}

func args(vs ...any) []json.RawMessage {
	out := make([]json.RawMessage, len(vs))
	for i, v := range vs {
		out[i], _ = json.Marshal(v)
	}
	return out
}

func must(t *testing.T, e *domain.Engine, who, name string, a ...any) any {
	t.Helper()
	v, err := e.Dispatch(e.User(who), name, args(a...), "")
	if err != nil {
		t.Fatalf("%s by %s: %v", name, who, err)
	}
	return v
}

func booking(e *domain.Engine, id string) *domain.Booking {
	for _, b := range e.Data().Bookings {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func addPhotos(e *domain.Engine, n int, stage string) []string {
	ids := []string{}
	for i := 0; i < n; i++ {
		p := &domain.PhotoRecord{ID: domain.NewID("p"), Stage: stage, UploaderID: "u-bayu", Src: "/img/x.webp", Sha256: "00", Source: "kamera", ServerTimestamp: e.Now()}
		e.AddPhoto(p)
		ids = append(ids, p.ID)
	}
	return ids
}

func TestFullRentalCycle(t *testing.T) {
	e := newEngine(t)
	day := func(n int) string { return domain.AddDays(domain.IsoDate(e.Now()), n) }
	var listing *domain.Listing
	for _, l := range e.Data().Listings {
		if l.ID == "l-kompor-lapangan" {
			listing = l
		}
	}
	id := must(t, e, "u-raka", "createBooking", "u-raka", map[string]any{"listingId": listing.ID, "startDate": day(1), "endDate": day(3), "handover": "jemput", "collateralOptionId": listing.CollateralOptions[0].ID}).(string)
	b := booking(e, id)
	if b.Status != "REQUESTED" || b.Agreement.Quote.Days != 2 || len(b.Agreement.Hash) != 64 {
		t.Fatalf("unexpected booking %+v", b.Status)
	}
	must(t, e, "u-bayu", "acceptBooking", "u-bayu", id)
	must(t, e, "u-raka", "payBooking", "u-raka", id, "qris")
	must(t, e, "u-bayu", "submitCheckout", "u-bayu", id, map[string]any{"photoIds": addPhotos(e, 3, "checkout"), "collateralPhotoIds": addPhotos(e, 1, "jaminan")})
	if booking(e, id).Status != "ACCEPTED" {
		t.Fatal("ACTIVE must wait for the renter's agreement")
	}
	must(t, e, "u-raka", "agreeCheckout", "u-raka", id)
	if booking(e, id).Status != "ACTIVE" {
		t.Fatalf("want ACTIVE, got %s", booking(e, id).Status)
	}
	must(t, e, "u-bayu", "submitCheckin", "u-bayu", id, map[string]any{"photoIds": addPhotos(e, 3, "checkin"), "result": "sesuai"})
	must(t, e, "u-bayu", "returnCollateral", "u-bayu", id)
	must(t, e, "u-raka", "confirmCollateral", "u-raka", id)
	if booking(e, id).Status != "COMPLETED" {
		t.Fatalf("want COMPLETED, got %s", booking(e, id).Status)
	}
	must(t, e, "u-raka", "submitReview", "u-raka", id, 5, "Mantap")
	if _, err := e.Dispatch(e.User("u-raka"), "submitReview", args("u-raka", id, 4, "lagi"), ""); err == nil {
		t.Fatal("second review must be rejected")
	}
}

func TestActorMustMatchSession(t *testing.T) {
	e := newEngine(t)
	_, err := e.Dispatch(e.User("u-raka"), "acceptBooking", args("u-bayu", "b-requested"), "")
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	_, err = e.Dispatch(e.User("u-raka"), "acceptBooking", args("u-raka", "b-requested"), "")
	var ue domain.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("renter accepting must be a rule violation, got %v", err)
	}
}

func TestUnverifiedCannotRequest(t *testing.T) {
	e := newEngine(t)
	_, err := e.Dispatch(e.User("u-bima"), "createBooking", args("u-bima", map[string]any{"listingId": "l-payung-besar"}), "")
	var ue domain.UserError
	if !errors.As(err, &ue) {
		t.Fatalf("FR-A6: unverified user must not request, got %v", err)
	}
}

func TestExpiryOverdueAndEscalation(t *testing.T) {
	e := newEngine(t)
	e.AdvanceClock(25 * domain.Hour)
	if s := booking(e, "b-requested").Status; s != "EXPIRED" {
		t.Fatalf("FR-R3: want EXPIRED after 24h, got %s", s)
	}
	if !booking(e, "b-overdue").Overdue {
		t.Fatal("overdue flag must stay set")
	}
}

func TestCommission(t *testing.T) {
	min, max := int64(2000), int64(50000)
	c := domain.CommissionConfig{Active: false, Mode: "persen", Percent: 10, Min: &min, Max: &max, Bearer: "split", SplitRenterPercent: 50}
	s := domain.ComputeCommission(15000, c)
	if s.Amount != 2000 || s.Charged != 0 {
		t.Fatalf("min applies and nothing is charged in pilot: %+v", s)
	}
	c.Active = true
	s = domain.ComputeCommission(1_000_000, c)
	if s.Amount != 50000 || s.RenterPays != 25000 || s.ProviderPays != 25000 {
		t.Fatalf("max + 50:50 split: %+v", s)
	}
}

func TestQuoteRentWeeklyAndDiscount(t *testing.T) {
	week := int64(200000)
	l := &domain.Listing{PricePerDay: 35000, PricePerWeek: &week, DurationDiscount: &domain.DurationDiscount{MinDays: 3, Percent: 10}}
	q := domain.QuoteRent(l, "2026-11-01", "2026-11-09") // 8 days = 1 week + 1 day
	if q.Days != 8 || q.Total != int64(float64(200000+35000)*0.9) {
		t.Fatalf("unexpected quote %+v", q)
	}
}

func TestBannedKeywords(t *testing.T) {
	if got := domain.BannedWordsIn("Airsoft gun replika", []string{"airsoft", "senjata"}); len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if got := domain.BannedWordsIn("Pisau dapur", []string{"senjata"}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}
