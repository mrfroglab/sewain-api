package service_test

import (
	"testing"
)

// Review wave 2 (E1): evidence photo ids were only checked for existence. A provider could submit the check-out of a
// new booking with photos taken for another booking (old condition, old server timestamps) or uploaded by someone
// else, which defeats the point of server-timestamped evidence (FR-P2/P3).
func TestEvidencePhotosBelongToTheBooking(t *testing.T) {
	e := newEnv(t)
	other := "b-returned" // u-dina is the provider there too
	old := e.photos(3, "checkout", "u-dina", &other)
	col := "b-accepted"
	jaminan := e.photos(1, "jaminan", "u-dina", &col)
	if _, err := e.try("u-dina", "submitCheckout", "u-dina", "b-accepted", map[string]any{"photoIds": old, "collateralPhotoIds": jaminan}); err == nil {
		t.Error("check-out with photos of another booking accepted")
	}
	foreign := e.photos(3, "checkout", "u-raka", nil) // uploaded by someone else, no booking
	if _, err := e.try("u-dina", "submitCheckout", "u-dina", "b-accepted", map[string]any{"photoIds": foreign, "collateralPhotoIds": jaminan}); err == nil {
		t.Error("check-out with photos uploaded by another user accepted")
	}
	ref := e.photos(3, "referensi", "u-dina", nil) // the listing's catalogue photos are not condition evidence
	if _, err := e.try("u-dina", "submitCheckout", "u-dina", "b-accepted", map[string]any{"photoIds": ref, "collateralPhotoIds": jaminan}); err == nil {
		t.Error("check-out with listing reference photos accepted")
	}
	if _, err := e.try("u-raka", "sendMessage", "u-raka", "b-accepted", map[string]any{"photoId": old[0]}); err == nil {
		t.Error("chat photo of another booking accepted")
	}
	if _, err := e.try("u-raka", "openDispute", "u-raka", "b-overdue", map[string]any{"description": "rusak", "photoIds": []string{"p-doesnotexist"}}); err == nil {
		t.Error("dispute with unknown photo ids accepted")
	}
	// Photos of this booking (or the actor's own photos without a booking) are fine.
	own := e.photos(3, "checkout", "u-dina", &col)
	e.must("u-dina", "submitCheckout", "u-dina", "b-accepted", map[string]any{"photoIds": own, "collateralPhotoIds": jaminan})
	loose := e.photos(1, "chat", "u-raka", nil)
	e.must("u-raka", "sendMessage", "u-raka", "b-accepted", map[string]any{"photoId": loose[0]})
}
