package service

import (
	"context"
	"time"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/media"
)

// PhotoStages accepted by the upload endpoints. KTP/selfie are never uploaded through these (PRD §7.2); the auth
// module has its own encrypted KYC upload.
var PhotoStages = map[string]bool{"referensi": true, "checkout": true, "checkin": true, "jaminan": true, "masalah": true, "chat": true, "sengketa": true}

// NewPhoto is the metadata of a stored photo (the bytes live in the photo store).
type NewPhoto struct {
	ID         string
	BookingID  *string
	Stage      string
	UploaderID string
	Src        string
	ObjectKey  *string
	Sha256     string
	Source     string
	Angle      *string
	Width      *int
	Height     *int
	Bytes      *int64
	Mime       *string
}

// AddPhoto registers evidence (server-side SHA-256 + server timestamp on the demo clock, FR-P2/P3). Photos are
// append-only (FR-P4): there is no update or delete.
func (s *Service) AddPhoto(ctx context.Context, p NewPhoto) (*domain.PhotoRecord, error) {
	var rec *domain.PhotoRecord
	_, err := s.run(ctx, func(t *txn) error {
		if p.ID == "" {
			p.ID = domain.NewID("p")
		}
		t.check(t.q.InsertPhoto(t.ctx, dbq.InsertPhotoParams{ID: p.ID, BookingID: p.BookingID, Stage: p.Stage, UploaderID: p.UploaderID, Src: p.Src,
			ObjectKey: p.ObjectKey, Sha256: p.Sha256, ServerTs: tsOf(t.now), Source: p.Source, Angle: p.Angle, Width: p.Width, Height: p.Height,
			Bytes: p.Bytes, Mime: p.Mime}))
		t.touch(p.UploaderID)
		if p.BookingID != nil {
			if b := t.booking(*p.BookingID); b != nil {
				t.touch(b.RenterID, b.ProviderID)
			}
		}
		rec = &domain.PhotoRecord{ID: p.ID, BookingID: p.BookingID, Stage: p.Stage, UploaderID: p.UploaderID, Src: p.Src, Sha256: p.Sha256,
			ServerTimestamp: t.now, Source: p.Source, Angle: p.Angle, Width: p.Width, Height: p.Height, Bytes: p.Bytes}
		return nil
	})
	return rec, err
}

// StoredPhoto is a photo row with its storage key.
type StoredPhoto struct {
	*domain.PhotoRecord
	ObjectKey *string
	Mime      *string
}

// Photo returns a photo by id (nil when unknown).
func (s *Service) Photo(ctx context.Context, id string) (*StoredPhoto, error) {
	r, err := s.q.GetPhoto(ctx, id)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &StoredPhoto{PhotoRecord: photoFromRow(r), ObjectKey: r.ObjectKey, Mime: r.Mime}, nil
}

// CanSeePhoto: KTP/selfie only for the uploader and admins; booking evidence (all stages with a booking) for the
// booking parties and admins (FR-J5, PRD 12.4); listing reference photos for everyone.
func (s *Service) CanSeePhoto(ctx context.Context, p *domain.PhotoRecord, viewer *Viewer) (bool, error) {
	if viewer.IsAdmin() {
		return true, nil
	}
	if p.Stage == "ktp" || p.Stage == "selfie" {
		return viewer != nil && viewer.UserID == p.UploaderID, nil
	}
	if p.BookingID == nil || p.Stage == "referensi" {
		return true, nil
	}
	if viewer == nil {
		return false, nil
	}
	if viewer.UserID == p.UploaderID {
		return true, nil
	}
	b, err := s.q.GetBooking(ctx, *p.BookingID)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return b.RenterID == viewer.UserID || b.ProviderID == viewer.UserID, nil
}

// LookupPhoto is the media handler's PhotoLookup (GET /api/v1/photos/{id}): the photo row with its booking (nil for
// listing photos). Returns media.ErrNotFound for unknown ids and for photos without a stored object (bundled demo
// images keep their static /img/… URL).
func (s *Service) LookupPhoto(ctx context.Context, id string) (media.Photo, *media.Booking, error) {
	var p media.Photo
	var objectKey, mime *string
	var bytes *int64
	var width, height *int
	var bID, bCode, bRenter, bProvider, bStatus *string
	var bCompleted, bResolved *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT p.id, p.stage, coalesce(p.booking_id, ''), coalesce(p.listing_id, ''), p.uploader_id, p.object_key, p.sha256, p.mime,
		       p.bytes, p.width, p.height, p.server_ts, b.id, b.code, b.renter_id, b.provider_id, b.status, b.completed_at, b.resolved_at
		FROM photos p LEFT JOIN bookings b ON b.id = p.booking_id
		WHERE p.id = $1`, id).Scan(&p.ID, &p.Stage, &p.BookingID, &p.ListingID, &p.UploaderID, &objectKey, &p.SHA256, &mime,
		&bytes, &width, &height, &p.ServerTime, &bID, &bCode, &bRenter, &bProvider, &bStatus, &bCompleted, &bResolved)
	if isNoRows(err) || (err == nil && objectKey == nil) {
		return media.Photo{}, nil, media.ErrNotFound
	}
	if err != nil {
		return media.Photo{}, nil, err
	}
	p.ObjectKey, p.MIME = *objectKey, domain.Deref(mime, "")
	if bytes != nil {
		p.Bytes = *bytes
	}
	if width != nil {
		p.Width = *width
	}
	if height != nil {
		p.Height = *height
	}
	if bID == nil {
		return p, nil, nil
	}
	return p, &media.Booking{ID: *bID, Code: *bCode, RenterID: *bRenter, ProviderID: *bProvider, Status: *bStatus,
		CompletedAt: bCompleted, ResolvedAt: bResolved}, nil
}

// CanUploadTo: only the parties of a booking (and admins, e.g. as mediator) attach photos to it.
func (s *Service) CanUploadTo(ctx context.Context, bookingID string, viewer *Viewer) (bool, error) {
	if viewer.IsAdmin() {
		return true, nil
	}
	if viewer == nil {
		return false, nil
	}
	b, err := s.q.GetBooking(ctx, bookingID)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return b.RenterID == viewer.UserID || b.ProviderID == viewer.UserID, nil
}
