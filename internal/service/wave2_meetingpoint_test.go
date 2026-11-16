package service_test

import (
	"testing"

	"github.com/mrfroglab/sewain-api/internal/service"
)

// Review wave 2 (V1): the listing's meeting point is a detail that opens only after ACCEPTED (FR-S6). Guests and
// other users must not receive it in the snapshot; the owner and admins do, and an accepted booking carries it.
func TestSnapshotHidesMeetingPointFromOthers(t *testing.T) {
	e := newEnv(t)
	scoped := scopedService(t, e)
	meeting := func(v *service.Viewer, id string) *string {
		t.Helper()
		snap, err := scoped.Snapshot(e.ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range snap.Listings {
			if l.ID == id {
				return l.MeetingPoint
			}
		}
		t.Fatalf("listing %s not in snapshot", id)
		return nil
	}
	if mp := meeting(nil, "l-carrier-60l"); mp != nil {
		t.Errorf("guest sees the meeting point: %q", *mp)
	}
	if mp := meeting(e.viewer("u-sinta"), "l-carrier-60l"); mp != nil {
		t.Errorf("other user sees the meeting point: %q", *mp)
	}
	// u-raka has a REQUESTED (not yet accepted) booking on this listing: still hidden.
	if mp := meeting(e.viewer("u-raka"), "l-carrier-60l"); mp != nil {
		t.Errorf("renter of a pending request sees the meeting point: %q", *mp)
	}
	if mp := meeting(e.viewer("u-bayu"), "l-carrier-60l"); mp == nil {
		t.Error("owner must see their own meeting point")
	}
	if mp := meeting(e.viewer("u-admin"), "l-carrier-60l"); mp == nil {
		t.Error("admin must see the meeting point")
	}
	// After acceptance the renter gets the detail on the booking.
	e.must("u-bayu", "acceptBooking", "u-bayu", "b-requested", nil)
	if b := e.booking("b-requested"); b.MeetingPointDetail == nil || *b.MeetingPointDetail == "" {
		t.Fatal("accepted booking must carry the meeting point detail")
	}
}
