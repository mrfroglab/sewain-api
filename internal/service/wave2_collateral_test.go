package service_test

import (
	"testing"
	"time"
)

// Review wave 2 (T1): confirmCollateral could be repeated: every call bumped the public rentCount of the listing and
// notified the provider again. The auto-complete job, on the other hand, never counted the rental.
func TestConfirmCollateralCountsOnce(t *testing.T) {
	e := newEnv(t)
	before := e.listing("l-tripod").RentCount
	e.must("u-dina", "returnCollateral", "u-dina", "b-returned")
	e.must("u-sinta", "confirmCollateral", "u-sinta", "b-returned")
	if b := e.booking("b-returned"); b.Status != "COMPLETED" {
		t.Fatalf("status %s", b.Status)
	}
	if msg := e.reject("u-sinta", "confirmCollateral", "u-sinta", "b-returned"); msg != "Jaminan sudah dikonfirmasi." {
		t.Fatalf("second confirmation: %q", msg)
	}
	for i := 0; i < 3; i++ {
		_, _ = e.try("u-sinta", "confirmCollateral", "u-sinta", "b-returned")
	}
	if got := e.listing("l-tripod").RentCount; got != before+1 {
		t.Fatalf("rentCount %d, want %d", got, before+1)
	}
	// A resolved booking (dispute) completes nothing: confirming the collateral does not count a rental.
	resolvedBefore := e.listing("l-action-cam").RentCount
	_, _ = e.try("u-sinta", "confirmCollateral", "u-sinta", "b-resolved")
	if got := e.listing("l-action-cam").RentCount; got != resolvedBefore {
		t.Fatalf("resolved booking counted: %d → %d", resolvedBefore, got)
	}
}

func TestAutoCompleteCountsRental(t *testing.T) {
	e := newEnv(t)
	before := e.listing("l-tripod").RentCount
	e.must("u-dina", "returnCollateral", "u-dina", "b-returned")
	e.clock.Add(49 * time.Hour)
	if _, err := e.svc.RunJobs(e.ctx); err != nil {
		t.Fatal(err)
	}
	if b := e.booking("b-returned"); b.Status != "COMPLETED" {
		t.Fatalf("status %s", b.Status)
	}
	if got := e.listing("l-tripod").RentCount; got != before+1 {
		t.Fatalf("auto-completed rental not counted: %d, want %d", got, before+1)
	}
	// The renter may still confirm afterwards (record), without counting again.
	e.must("u-sinta", "confirmCollateral", "u-sinta", "b-returned")
	if got := e.listing("l-tripod").RentCount; got != before+1 {
		t.Fatalf("late confirmation counted again: %d", got)
	}
}
