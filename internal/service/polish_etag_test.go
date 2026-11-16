package service_test

import (
	"testing"

	"github.com/mrfroglab/sewain-api/internal/service"
)

// Load test finding 7.2: the ETag follows the changes relevant to the viewer, not the global version. User B's
// private changes (favorites, chat, notifications) keep user A's and a guest's ETag, so their refetch gets 304; a
// public change or a change on A's own data moves A's ETag. Admins see everything, so every change moves theirs.
func TestETagPerViewer(t *testing.T) {
	e := newEnv(t)
	svc := scopedService(t, e)
	raka, guest := &service.Viewer{UserID: "u-raka", Role: "user"}, (*service.Viewer)(nil)
	admin := &service.Viewer{UserID: "u-admin", Role: "admin"}
	etag := func(v *service.Viewer) string {
		t.Helper()
		tag, _, err := svc.ETag(e.ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		// The snapshot carries the same viewer version, so the ETag sent with a 200 matches the 304 check.
		snap, err := svc.Snapshot(e.ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		if svc.ETagFor(v, snap.ViewerVersion, snap.ClockOffset) != tag {
			t.Fatal("snapshot ETag differs from the pre-check")
		}
		return tag
	}
	r0, g0, a0 := etag(raka), etag(guest), etag(admin)

	// Sinta's private changes: favorite, chat with Bayu, reading notifications.
	e.must("u-sinta", "toggleFavorite", "u-sinta", "l-headlamp")
	e.must("u-sinta", "sendMessage", "u-sinta", "b-active", map[string]any{"text": "Besok aku kembalikan"})
	e.must("u-sinta", "markNotificationsRead", "u-sinta", []string{})
	e.must("u-sinta", "markThreadRead", "u-sinta", "b-active")
	if etag(raka) != r0 || etag(guest) != g0 {
		t.Fatal("another user's private change moved the ETag")
	}
	a1 := etag(admin)
	if a1 == a0 {
		t.Fatal("admin ETag must follow every change")
	}

	// A change on Raka's own data moves only his (and the admin's).
	e.must("u-raka", "toggleFavorite", "u-raka", "l-headlamp")
	r1 := etag(raka)
	if r1 == r0 || etag(guest) != g0 {
		t.Fatal("own change: raka must move, guest must not")
	}
	// Someone writing to Raka (a chat on his booking) moves his ETag.
	e.must("u-bayu", "sendMessage", "u-bayu", "b-requested", map[string]any{"text": "Bisa, kak"})
	r2 := etag(raka)
	if r2 == r1 {
		t.Fatal("message to raka did not move his ETag")
	}
	// Public data (configuration, listings, profiles, listing calendars) moves everyone's.
	e.must("u-admin", "updateCommission", "u-admin", map[string]any{"percent": 7})
	if etag(raka) == r2 || etag(guest) == g0 {
		t.Fatal("public change must move every ETag")
	}
}
