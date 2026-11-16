package service_test

import (
	"testing"
)

// Review wave 2 (C2): a booking under dispute while the item is still with the renter (DISPUTED, not returned) did not
// hold stock: the same unit could be requested and accepted again for overlapping dates, although the snapshot's
// listingBusy already shows those days as taken.
func TestDisputedBookingHoldsStock(t *testing.T) {
	e := newEnv(t)
	l := e.listing("l-tripod") // u-dina, stock 1
	id := e.must("u-sinta", "createBooking", "u-sinta", bookingInput(l, e.today(0), e.today(3))).(string)
	e.must("u-dina", "acceptBooking", "u-dina", id, nil)
	photos := e.photos(3, "checkout", "u-dina", &id)
	jaminan := e.photos(1, "jaminan", "u-dina", &id)
	e.must("u-dina", "submitCheckout", "u-dina", id, map[string]any{"photoIds": photos, "collateralPhotoIds": jaminan})
	e.must("u-sinta", "agreeCheckout", "u-sinta", id)
	e.must("u-sinta", "openDispute", "u-sinta", id, map[string]any{"caseType": "lainnya", "description": "Kaki tripod macet"})
	if b := e.booking(id); b.Status != "DISPUTED" {
		t.Fatalf("status %s", b.Status)
	}
	if _, err := e.try("u-raka", "createBooking", "u-raka", bookingInput(l, e.today(1), e.today(2))); err == nil {
		t.Fatal("unit under dispute (still with the renter) was requested again for overlapping dates")
	}
}
