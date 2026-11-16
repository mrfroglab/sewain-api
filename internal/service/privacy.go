package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/domain"
)

// DeletedUserName replaces the name of a deleted (anonymized) account.
const DeletedUserName = "Pengguna terhapus"

// Export is GET /api/v1/me/export (UU PDP right of access): everything the user owns. Other people appear only by id
// and public name; their messages, profiles and evidence photos are left out.
func (s *Service) Export(ctx context.Context, userID string) (map[string]any, error) {
	snap, err := s.snapshot(ctx, &Viewer{UserID: userID, Role: "user"}, ScopeScoped)
	if err != nil {
		return nil, err
	}
	d := snap.Data
	var me *domain.User
	names := map[string]*domain.User{}
	for _, u := range d.Users {
		names[u.ID] = u
		if u.ID == userID {
			cp := *u
			cp.PublicStats = nil
			me = &cp
		}
	}
	if me == nil {
		return nil, nil
	}
	others := map[string]string{}
	other := func(id string) {
		if id != "" && id != userID && id != "system" {
			others[id] = domain.PublicName(names[id])
		}
	}
	out := map[string]any{"exportedAt": snap.ServerNow, "profile": me}
	listings := []*domain.Listing{}
	for _, l := range d.Listings {
		if l.OwnerID == userID {
			listings = append(listings, l)
		}
	}
	out["listings"] = listings
	for _, b := range d.Bookings {
		other(b.RenterID)
		other(b.ProviderID)
	}
	out["bookings"] = d.Bookings
	photos := []*domain.PhotoRecord{}
	for _, p := range d.Photos {
		if p.UploaderID == userID {
			photos = append(photos, p)
		}
	}
	out["photos"] = photos
	messages := []*domain.Message{}
	for _, m := range d.Messages {
		if m.SenderID == userID || m.SenderID == "system" || m.SenderID == "" {
			messages = append(messages, m)
		}
	}
	out["messages"] = messages
	reviews := []*domain.Review{}
	for _, r := range d.Reviews {
		if r.FromID == userID || r.ToID == userID {
			other(r.FromID)
			other(r.ToID)
			reviews = append(reviews, r)
		}
	}
	out["reviews"] = reviews
	wanted := []*domain.WantedPost{}
	for _, w := range d.Wanted {
		if w.RequesterID == userID {
			wanted = append(wanted, w)
		}
	}
	out["wanted"] = wanted
	out["verifications"] = d.Verifications
	out["notifications"] = d.Notifications
	out["favorites"] = d.Favorites[userID]
	out["reports"] = d.Reports
	out["strikes"] = d.Strikes
	out["disputes"] = d.Disputes
	reads := map[string]int64{}
	for k, v := range d.ChatReads {
		if strings.HasPrefix(k, userID+":") {
			reads[k] = v
		}
	}
	out["chatReads"] = reads
	out["counterparts"] = others

	// Module data (auth, KYC, notify) without secrets: no token hashes, no NIK, no push keys.
	extra := map[string]string{
		"consents": `SELECT coalesce(jsonb_agg(jsonb_build_object('doc', doc, 'version', version, 'acceptedAt', accepted_at) ORDER BY id), '[]')
			FROM consents WHERE user_id = $1`,
		"sessions": `SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'createdAt', created_at, 'lastSeenAt', last_seen_at,
			'expiresAt', expires_at, 'revokedAt', revoked_at, 'userAgent', user_agent, 'ip', ip) ORDER BY created_at), '[]')
			FROM auth_sessions WHERE user_id = $1`,
		"kycSubmissions": `SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'status', status, 'nikMasked', nik_masked, 'reason', reason,
			'createdAt', created_at, 'decidedAt', decided_at, 'filesPurgedAt', files_purged_at) ORDER BY created_at), '[]')
			FROM kyc_submissions WHERE user_id = $1`,
		"pushSubscriptions": `SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'service', split_part(endpoint, '/', 3),
			'userAgent', user_agent, 'createdAt', created_at) ORDER BY created_at), '[]')
			FROM push_subscriptions WHERE user_id = $1`,
		"notificationPrefs": `SELECT coalesce((SELECT to_jsonb(p) - 'user_id' FROM notification_prefs p WHERE user_id = $1), 'null'::jsonb)`,
	}
	for key, q := range extra {
		var raw json.RawMessage
		if err := s.pool.QueryRow(ctx, q, userID).Scan(&raw); err != nil {
			return nil, err
		}
		out[key] = raw
	}
	return out, nil
}

// DeleteAccount is DELETE /api/v1/me (UU PDP right to erasure) as anonymisation: bookings, reviews and evidence stay
// for the other party, but the profile loses name, phone, e-mail, bio and NIK, sessions/push/preferences/
// notifications are deleted, own listings are taken down and wanted posts closed. Refused while a booking is still
// running or a dispute is open.
func (s *Service) DeleteAccount(ctx context.Context, userID string) (Change, error) {
	return s.run(ctx, func(t *txn) error {
		u := t.userForUpdate(userID)
		if u == nil {
			return domain.Fail("Akun tidak ditemukan.")
		}
		var running, disputes int
		err := t.tx.QueryRow(t.ctx, `SELECT
			  (SELECT count(*) FROM bookings WHERE (renter_id = $1 OR provider_id = $1)
			     AND status IN ('REQUESTED', 'ACCEPTED', 'ACTIVE', 'RETURNED', 'DISPUTED')),
			  (SELECT count(*) FROM disputes d JOIN bookings b ON b.id = d.booking_id
			     WHERE (b.renter_id = $1 OR b.provider_id = $1) AND d.status <> 'selesai')`, userID).Scan(&running, &disputes)
		if !t.check(err) {
			return nil
		}
		if disputes > 0 {
			return domain.Fail("Masih ada sengketa yang terbuka. Selesaikan dulu sebelum menghapus akun.")
		}
		if running > 0 {
			return domain.Fail("Masih ada sewaan yang berjalan. Selesaikan atau batalkan dulu sebelum menghapus akun.")
		}
		var pendingKYC bool
		err = t.tx.QueryRow(t.ctx, `SELECT EXISTS (SELECT 1 FROM kyc_submissions WHERE user_id = $1 AND status = 'pending')`, userID).Scan(&pendingKYC)
		if !t.check(err) {
			return nil
		}
		if pendingKYC {
			return domain.Fail("Pengajuan verifikasimu masih ditinjau admin. Coba lagi setelah ada keputusan.")
		}
		oldPhone := u.Phone
		stmts := []struct {
			sql  string
			args []any
		}{
			{`UPDATE users SET name = $2, nickname = NULL, phone = '', phone_verified_at = NULL, email = NULL, bio = NULL, campus = NULL,
			    nik_masked = NULL, demo = NULL, deleted_at = $3, updated_at = now() WHERE id = $1`, []any{userID, DeletedUserName, tsOf(t.now)}},
			{`DELETE FROM stores WHERE user_id = $1`, []any{userID}},
			{`DELETE FROM auth_sessions WHERE user_id = $1`, []any{userID}},
			{`DELETE FROM push_subscriptions WHERE user_id = $1`, []any{userID}},
			{`DELETE FROM notification_prefs WHERE user_id = $1`, []any{userID}},
			{`DELETE FROM notification_outbox WHERE user_id = $1`, []any{userID}},
			{`DELETE FROM notifications WHERE user_id = $1`, []any{userID}},
			{`DELETE FROM favorites WHERE user_id = $1`, []any{userID}},
			{`DELETE FROM identities WHERE user_id = $1`, []any{userID}},
			{`UPDATE kyc_submissions SET files_purge_after = least(coalesce(files_purge_after, $2), $2) WHERE user_id = $1 AND files_purged_at IS NULL`,
				[]any{userID, tsOf(t.now)}},
			{`UPDATE listings SET status = 'diturunkan', updated_at = now() WHERE owner_id = $1`, []any{userID}},
			{`UPDATE wanted_posts SET status = 'ditutup', updated_at = now() WHERE requester_id = $1 AND status = 'terbuka'`, []any{userID}},
		}
		if oldPhone != "" {
			stmts = append(stmts, struct {
				sql  string
				args []any
			}{`DELETE FROM otp_challenges WHERE phone = $1`, []any{"+62" + strings.TrimPrefix(oldPhone, "0")}})
		}
		for _, st := range stmts {
			if _, err := t.tx.Exec(t.ctx, st.sql, st.args...); !t.check(err) {
				return nil
			}
		}
		t.touch(userID)
		t.touchPublic()
		t.audit(userID, "akun.hapus", userID, nil, nil)
		return nil
	})
}

// IsDeleted reports whether the account was deleted.
func (s *Service) IsDeleted(ctx context.Context, userID string) (bool, error) {
	var deleted bool
	err := s.pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&deleted)
	if err == pgx.ErrNoRows {
		return true, nil
	}
	return deleted, err
}
