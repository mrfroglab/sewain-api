package service_test

import (
	"testing"

	"github.com/mrfroglab/sewain-api/internal/service"
)

// Review wave 2 (M1): money and range fields of a listing were not bounded. A late-fee cap above 100% of the item
// value (PRD: denda telat maks 100%), a duration discount above 100% (negative rent), a negative late fee, a deposit
// above the item value, or prices that overflow int64 over a long booking were all accepted.
func TestListingMoneyAndRangeBounds(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name string
		mod  func(map[string]any)
	}{
		{"late fee cap 500%", func(in map[string]any) {
			in["rules"] = map[string]any{"lateFeePerDay": 10000, "lateFeeCapPercent": 500, "cancellation": "x"}
		}},
		{"negative late fee", func(in map[string]any) {
			in["rules"] = map[string]any{"lateFeePerDay": -10000, "lateFeeCapPercent": 100, "cancellation": "x"}
		}},
		{"discount 150%", func(in map[string]any) { in["durationDiscount"] = map[string]any{"minDays": 2, "percent": 150} }},
		{"negative weekly price", func(in map[string]any) { in["pricePerWeek"] = -1 }},
		{"giant daily price", func(in map[string]any) { in["pricePerDay"] = int64(4_000_000_000_000_000_000) }},
		{"giant item value", func(in map[string]any) { in["itemValue"] = int64(9_000_000_000_000_000_000) }},
		{"deposit 300%", func(in map[string]any) {
			in["collateralOptions"] = []map[string]any{{"id": "co-x1", "kind": "deposit", "depositMode": "persen", "depositPercent": 300}}
		}},
		{"negative deposit", func(in map[string]any) {
			in["collateralOptions"] = []map[string]any{{"id": "co-x1", "kind": "deposit", "depositMode": "tetap", "depositAmount": -50000}}
		}},
		{"radius 10000 km", func(in map[string]any) { in["radiusKm"] = 10000 }},
		{"title of 10k chars", func(in map[string]any) {
			b := make([]byte, 10000)
			for i := range b {
				b[i] = 'a'
			}
			in["title"] = string(b)
		}},
		{"min days above max days", func(in map[string]any) { in["minDays"] = 10; in["maxDays"] = 2 }},
	}
	for _, c := range cases {
		in := listingInput("Tenda kecil 2 orang")
		c.mod(in)
		if _, err := e.try("u-raka", "createListing", "u-raka", in); err == nil {
			t.Errorf("%s: accepted", c.name)
		} else if _, ok := service.IsUserError(err); !ok {
			t.Errorf("%s: want a rule violation, got %v", c.name, err)
		}
	}
}

// Bookings far in the future (or absurdly long) are refused: the rent overflows and stock is blocked for years.
func TestBookingDateHorizon(t *testing.T) {
	e := newEnv(t)
	l := e.listing("l-kompor-lapangan")
	if _, err := e.try("u-raka", "createBooking", "u-raka", bookingInput(l, e.today(1), "9999-12-31")); err == nil {
		t.Fatal("booking until 9999 accepted")
	}
	if _, err := e.try("u-raka", "createBooking", "u-raka", bookingInput(l, e.today(800), e.today(801))); err == nil {
		t.Fatal("booking two years ahead accepted")
	}
}
