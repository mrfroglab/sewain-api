package service_test

import (
	"testing"
)

// Review wave 2 (B1): createListing forces manual approval for high-value items/vehicles (PRD 11.4) and stock 1 for
// individual accounts, but updateListing applied the payload as-is.
func TestUpdateListingKeepsApprovalAndStockRules(t *testing.T) {
	e := newEnv(t)
	id := e.must("u-raka", "createListing", "u-raka", listingInput("Tenda kecil 2 orang")).(string)
	in := listingInput("Tenda kecil 2 orang")
	in["itemValue"] = 50_000_000
	in["approval"] = "otomatis"
	in["stock"] = 5
	e.must("u-raka", "updateListing", "u-raka", id, in)
	l := e.listing(id)
	if l.Approval != "manual" {
		t.Errorf("high-value item switched to auto-approval: %s", l.Approval)
	}
	if l.Stock != 1 {
		t.Errorf("individual account raised stock to %d", l.Stock)
	}
}
