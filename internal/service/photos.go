package service

import (
	"context"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
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
