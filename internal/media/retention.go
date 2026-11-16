package media

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// EvidenceRetention is how long booking evidence is kept after the booking is finished (FR-P8).
const EvidenceRetention = 90 * 24 * time.Hour

// Dispute is the subset of a dispute that retention needs. ResolvedAt == nil means still open.
type Dispute struct {
	ID         string
	BookingID  string
	Status     string
	ResolvedAt *time.Time
}

// PlanPhotoRetention returns the IDs of evidence photos that may be deleted at now, sorted.
//
// A photo is eligible when its booking is finished, COMPLETED (clock starts at CompletedAt) or RESOLVED
// after a dispute (clock starts at ResolvedAt), at least EvidenceRetention ago, and the booking has no
// open dispute. A dispute resolved later than the booking moves the start of the clock to its resolution.
// Listing (referensi) photos, KYC files and photos without a known booking are never planned here;
// unknown or missing data always means "keep".
func PlanPhotoRetention(now time.Time, photos []Photo, bookings []Booking, disputes []Dispute) []string {
	finished := make(map[string]time.Time, len(bookings))
	for _, b := range bookings {
		var at *time.Time
		switch b.Status {
		case "COMPLETED":
			at = b.CompletedAt
		case "RESOLVED":
			at = b.ResolvedAt
			if at == nil {
				at = b.CompletedAt
			}
		}
		if at != nil && !at.IsZero() {
			finished[b.ID] = *at
		}
	}
	blocked := map[string]bool{}
	for _, d := range disputes {
		if d.ResolvedAt == nil || d.ResolvedAt.IsZero() {
			blocked[d.BookingID] = true
			continue
		}
		if at, ok := finished[d.BookingID]; ok && d.ResolvedAt.After(at) {
			finished[d.BookingID] = *d.ResolvedAt
		}
	}
	var ids []string
	for _, p := range photos {
		if !IsEvidenceStage(p.Stage) || p.BookingID == "" || blocked[p.BookingID] {
			continue
		}
		at, ok := finished[p.BookingID]
		if !ok || now.Before(at.Add(EvidenceRetention)) {
			continue
		}
		ids = append(ids, p.ID)
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// RetentionKeys lists every storage object derived from a photo: the original and the watermark cache.
func RetentionKeys(p Photo) []string {
	keys := []string{WatermarkKey(p.ID)}
	if p.ObjectKey != "" {
		keys = append(keys, p.ObjectKey)
	}
	return keys
}

// Purge deletes keys from st. Missing objects count as deleted. It keeps going after a failure and
// returns how many keys were removed plus all errors joined, so one bad object cannot stall the job.
func Purge(ctx context.Context, st Storage, keys []string) (int, error) {
	var errs []error
	n := 0
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := st.Delete(ctx, k); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", k, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
