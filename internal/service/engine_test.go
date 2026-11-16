package service_test

import (
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

// Ported from v1 internal/domain/engine_test.go.

func TestFullRentalCycle(t *testing.T) {
	e := newEnv(t)
	l := e.listing("l-kompor-lapangan")
	id := e.must("u-raka", "createBooking", "u-raka", map[string]any{"listingId": l.ID, "startDate": e.today(1), "endDate": e.today(3), "handover": "jemput", "collateralOptionId": l.CollateralOptions[0].ID}).(string)
	b := e.booking(id)
	if b.Status != "REQUESTED" || b.Agreement.Quote.Days != 2 || len(b.Agreement.Hash) != 64 {
		t.Fatalf("unexpected booking %+v", b.Status)
	}
	if b.Agreement.Hash != domain.AgreementHash(b.Agreement) {
		t.Fatal("stored agreement must still match its hash")
	}
	e.must("u-bayu", "acceptBooking", "u-bayu", id)
	e.must("u-raka", "payBooking", "u-raka", id, "qris")
	e.must("u-bayu", "submitCheckout", "u-bayu", id, map[string]any{"photoIds": e.photos(3, "checkout", "u-bayu", &id), "collateralPhotoIds": e.photos(1, "jaminan", "u-bayu", &id)})
	if e.booking(id).Status != "ACCEPTED" {
		t.Fatal("ACTIVE must wait for the renter's agreement")
	}
	e.must("u-raka", "agreeCheckout", "u-raka", id)
	if s := e.booking(id).Status; s != "ACTIVE" {
		t.Fatalf("want ACTIVE, got %s", s)
	}
	e.must("u-bayu", "submitCheckin", "u-bayu", id, map[string]any{"photoIds": e.photos(3, "checkin", "u-bayu", &id), "result": "sesuai"})
	e.must("u-bayu", "returnCollateral", "u-bayu", id)
	e.must("u-raka", "confirmCollateral", "u-raka", id)
	if s := e.booking(id).Status; s != "COMPLETED" {
		t.Fatalf("want COMPLETED, got %s", s)
	}
	e.must("u-raka", "submitReview", "u-raka", id, 5, "Mantap")
	if msg := e.reject("u-raka", "submitReview", "u-raka", id, 4, "lagi"); msg != "Kamu sudah memberi rating." {
		t.Fatalf("second review: %q", msg)
	}
}

func TestActorMustMatchSession(t *testing.T) {
	e := newEnv(t)
	if _, err := e.try("u-raka", "acceptBooking", "u-bayu", "b-requested"); !isForbidden(err) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	if msg := e.reject("u-raka", "acceptBooking", "u-raka", "b-requested"); msg != "Booking tidak ditemukan." {
		t.Fatalf("renter accepting: %q", msg)
	}
}

func TestUnverifiedCannotRequest(t *testing.T) {
	e := newEnv(t)
	if msg := e.reject("u-bima", "createBooking", "u-bima", map[string]any{"listingId": "l-payung-besar"}); msg != "Verifikasi KTP + selfie dulu sebelum menyewa atau menyewakan." {
		t.Fatalf("FR-A6: %q", msg)
	}
}

func TestExpiryOverdueAndEscalation(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.AdvanceClock(e.ctx, 25*domain.Hour); err != nil {
		t.Fatal(err)
	}
	if s := e.booking("b-requested").Status; s != "EXPIRED" {
		t.Fatalf("FR-R3: want EXPIRED after 24h, got %s", s)
	}
	if !e.booking("b-overdue").Overdue {
		t.Fatal("overdue flag must stay set")
	}
}

func TestRealClockDrivesJobs(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.RunJobs(e.ctx); err != nil {
		t.Fatal(err)
	}
	if s := e.booking("b-requested").Status; s != "REQUESTED" {
		t.Fatalf("nothing expires yet, got %s", s)
	}
	e.clock.Add(25 * time.Hour)
	if _, err := e.svc.RunJobs(e.ctx); err != nil {
		t.Fatal(err)
	}
	if s := e.booking("b-requested").Status; s != "EXPIRED" {
		t.Fatalf("want EXPIRED, got %s", s)
	}
}
