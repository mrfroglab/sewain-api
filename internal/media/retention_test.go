package media

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestPlanPhotoRetention(t *testing.T) {
	day := 24 * time.Hour
	now := time.Date(2027, 3, 1, 3, 0, 0, 0, time.UTC) // fake clock
	at := func(daysAgo int) *time.Time { v := now.Add(-time.Duration(daysAgo) * day); return &v }

	bookings := []Booking{
		{ID: "b-old", Status: "COMPLETED", CompletedAt: at(91)},
		{ID: "b-edge", Status: "COMPLETED", CompletedAt: at(90)},
		{ID: "b-young", Status: "COMPLETED", CompletedAt: at(89)},
		{ID: "b-active", Status: "ACTIVE"},
		{ID: "b-open-dispute", Status: "COMPLETED", CompletedAt: at(200)},
		{ID: "b-disputed", Status: "DISPUTED", CompletedAt: at(200)},
		{ID: "b-resolved-old", Status: "RESOLVED", ResolvedAt: at(120)},
		{ID: "b-resolved-late", Status: "COMPLETED", CompletedAt: at(150)},
		{ID: "b-no-time", Status: "COMPLETED"},
	}
	disputes := []Dispute{
		{ID: "d-1", BookingID: "b-open-dispute", Status: "mediasi"},
		{ID: "d-2", BookingID: "b-disputed", Status: "negosiasi"},
		{ID: "d-3", BookingID: "b-resolved-old", Status: "selesai", ResolvedAt: at(120)},
		{ID: "d-4", BookingID: "b-resolved-late", Status: "selesai", ResolvedAt: at(30)}, // resolved 30 days ago
	}
	photos := []Photo{
		{ID: "p-old-checkout", Stage: StageCheckout, BookingID: "b-old"},
		{ID: "p-old-chat", Stage: StageChat, BookingID: "b-old"},
		{ID: "p-edge", Stage: StageCheckin, BookingID: "b-edge"},
		{ID: "p-young", Stage: StageCheckin, BookingID: "b-young"},
		{ID: "p-active", Stage: StageCheckout, BookingID: "b-active"},
		{ID: "p-open-dispute", Stage: StageSengketa, BookingID: "b-open-dispute"},
		{ID: "p-disputed", Stage: StageJaminan, BookingID: "b-disputed"},
		{ID: "p-resolved-old", Stage: StageSengketa, BookingID: "b-resolved-old"},
		{ID: "p-resolved-late", Stage: StageCheckout, BookingID: "b-resolved-late"},
		{ID: "p-no-time", Stage: StageCheckout, BookingID: "b-no-time"},
		{ID: "p-unknown-booking", Stage: StageCheckout, BookingID: "b-gone"},
		{ID: "p-listing", Stage: StageReferensi, ListingID: "l-1"},
		{ID: "p-ktp", Stage: "ktp", BookingID: "b-old"},
	}
	got := PlanPhotoRetention(now, photos, bookings, disputes)
	want := []string{"p-edge", "p-old-chat", "p-old-checkout", "p-resolved-old"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// Sixty days later the late-resolved dispute's photos become eligible too.
	got = PlanPhotoRetention(now.Add(60*day), photos, bookings, disputes)
	if !slices.Contains(got, "p-resolved-late") || !slices.Contains(got, "p-young") || slices.Contains(got, "p-open-dispute") {
		t.Fatalf("later plan = %v", got)
	}
}

type failingDelete struct{ Storage }

func (f failingDelete) Delete(ctx context.Context, key string) error {
	if key == "photos/bad.jpg" {
		return errors.New("access denied")
	}
	return f.Storage.Delete(ctx, key)
}

func TestPurge(t *testing.T) {
	st, _ := NewLocalStorage(t.TempDir())
	defer st.Close()
	ctx := context.Background()
	p := Photo{ID: "p-1", ObjectKey: "photos/p-1.jpg"}
	_ = st.Put(ctx, p.ObjectKey, []byte("a"), "image/jpeg")
	_ = st.Put(ctx, WatermarkKey(p.ID), []byte("b"), "image/jpeg")
	_ = st.Put(ctx, "photos/bad.jpg", []byte("c"), "image/jpeg")

	keys := append(RetentionKeys(p), "photos/bad.jpg", "photos/already-gone.jpg")
	n, err := Purge(ctx, failingDelete{st}, keys)
	if n != 3 || err == nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, k := range RetentionKeys(p) {
		if ok, _ := st.Exists(ctx, k); ok {
			t.Fatalf("%s still exists", k)
		}
	}
	if ok, _ := st.Exists(ctx, "photos/bad.jpg"); !ok {
		t.Fatal("failed key vanished")
	}
}
