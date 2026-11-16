package service

import (
	"context"
	"fmt"
	"time"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/media"
)

// Retention periods (F3.6, UU PDP data minimisation).
const (
	ReadNotificationRetention = 180 * 24 * time.Hour
	IdempotencyKeyRetention   = 24 * time.Hour
)

// DemoNow is the demo clock as a time.Time (real time + stored offset). Retention of rows stamped on the demo clock
// (bookings, notifications) uses it so a demo clock jump behaves like real time passing.
func (s *Service) DemoNow(ctx context.Context) (time.Time, error) {
	ms, err := s.Now(ctx)
	return time.UnixMilli(ms).UTC(), err
}

// PurgeEvidencePhotos deletes booking evidence 90 days after the booking finished, unless a dispute holds it
// (FR-P8, media.PlanPhotoRetention decides). Objects are deleted first (original + watermark cache); only photos
// whose objects are all gone lose their row, inside a transaction that opts in with SET LOCAL sewain.retention.
func (s *Service) PurgeEvidencePhotos(ctx context.Context, st media.Storage, now time.Time) (int, error) {
	cutoff := now.Add(-media.EvidenceRetention)
	bookings := []media.Booking{}
	brows, err := s.pool.Query(ctx, `SELECT id, code, renter_id, provider_id, status, completed_at, resolved_at FROM bookings
		WHERE status IN ('COMPLETED', 'RESOLVED') AND coalesce(resolved_at, completed_at) <= $1`, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []string
	for brows.Next() {
		var b media.Booking
		if err := brows.Scan(&b.ID, &b.Code, &b.RenterID, &b.ProviderID, &b.Status, &b.CompletedAt, &b.ResolvedAt); err != nil {
			brows.Close()
			return 0, err
		}
		bookings = append(bookings, b)
		ids = append(ids, b.ID)
	}
	brows.Close()
	if err := brows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	disputes := []media.Dispute{}
	drows, err := s.pool.Query(ctx, `SELECT id, booking_id, status, resolved_at FROM disputes WHERE booking_id = ANY($1)`, ids)
	if err != nil {
		return 0, err
	}
	for drows.Next() {
		var d media.Dispute
		if err := drows.Scan(&d.ID, &d.BookingID, &d.Status, &d.ResolvedAt); err != nil {
			drows.Close()
			return 0, err
		}
		disputes = append(disputes, d)
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return 0, err
	}
	photos := map[string]media.Photo{}
	var list []media.Photo
	prows, err := s.pool.Query(ctx, `SELECT id, stage, booking_id, uploader_id, coalesce(object_key, '') FROM photos WHERE booking_id = ANY($1)`, ids)
	if err != nil {
		return 0, err
	}
	for prows.Next() {
		var p media.Photo
		if err := prows.Scan(&p.ID, &p.Stage, &p.BookingID, &p.UploaderID, &p.ObjectKey); err != nil {
			prows.Close()
			return 0, err
		}
		photos[p.ID] = p
		list = append(list, p)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return 0, err
	}
	var gone []string
	var purgeErr error
	for _, id := range media.PlanPhotoRetention(now, list, bookings, disputes) {
		p := photos[id]
		keys := media.RetentionKeys(p)
		if n, err := media.Purge(ctx, st, keys); err != nil || n != len(keys) {
			purgeErr = fmt.Errorf("purge photo %s: %w", id, err)
			continue
		}
		gone = append(gone, id)
	}
	if len(gone) == 0 {
		return 0, purgeErr
	}
	var deleted int64
	_, err = s.run(ctx, func(t *txn) error {
		if _, err := t.tx.Exec(t.ctx, `SET LOCAL sewain.retention = 'on'`); !t.check(err) {
			return nil
		}
		tag, err := t.tx.Exec(t.ctx, `DELETE FROM photos WHERE id = ANY($1)`, gone)
		if !t.check(err) {
			return nil
		}
		deleted = tag.RowsAffected()
		for _, id := range gone {
			if b := t.booking(photos[id].BookingID); b != nil {
				t.touch(b.RenterID, b.ProviderID)
			}
		}
		t.audit("system", "retensi.foto", fmt.Sprintf("%d foto bukti", deleted), nil, domain.Ptr(gone[0]+" … "+gone[len(gone)-1]))
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(deleted), purgeErr
}

// PurgeReadNotifications deletes in-app notifications read more than 180 days ago (demo clock).
func (s *Service) PurgeReadNotifications(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM notifications WHERE read_at IS NOT NULL AND read_at < $1`, now.Add(-ReadNotificationRetention))
	return tag.RowsAffected(), err
}

// PurgeDeletedAccountsKYC removes KYC submission rows (encrypted NIK + name) of deleted accounts once their files are
// purged. Identities of deleted accounts were already removed when the account was deleted.
func (s *Service) PurgeDeletedAccountsKYC(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM kyc_submissions k USING users u
		WHERE u.id = k.user_id AND u.deleted_at IS NOT NULL AND k.status <> 'pending' AND k.ktp_object IS NULL AND k.selfie_object IS NULL`)
	return tag.RowsAffected(), err
}
