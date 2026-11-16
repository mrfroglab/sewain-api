package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

// KYCEvent is what the core needs from a KYC submission (internal/kyc.Submission) to mirror it into its own tables.
type KYCEvent struct {
	SubmissionID string
	UserID       string
	NIKMasked    string
	Status       string // pending | approved | rejected
	Reason       string
	DecidedAt    *time.Time
	DecidedBy    string // admin user id or "system:<provider>"
}

// KYCDecider decides a KYC submission through the KYC module (one decision path, audited there). The service calls
// it for reviewVerification on requests that came from a real KYC upload.
type KYCDecider func(ctx context.Context, adminID, submissionID string, approve bool, reason string) error

// SetKYCDecider wires the KYC module's decision path.
func (s *Service) SetKYCDecider(fn KYCDecider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kycDecider = fn
}

// inTx runs core logic inside a transaction owned by another module (KYC hooks) and publishes the change with
// PublishTx (delivered only if that transaction commits). A domain.UserError aborts the caller's transaction.
func (s *Service) inTx(ctx context.Context, tx pgx.Tx, fn func(t *txn) error) error {
	t := newTxn(ctx, s, tx)
	if err := t.loadClock(); err != nil {
		return err
	}
	if err := fn(t); err != nil {
		return err
	}
	if t.err != nil {
		return t.err
	}
	ch, err := t.finish()
	if err != nil {
		return err
	}
	return s.PublishTx(ctx, tx, ch)
}

// KYCSubmitted mirrors a new KYC submission (production flow, POST /api/v1/kyc/submissions): a verification request
// with method "manual" pointing at the submission, the user "menunggu", and the admin queue notified. Runs in the
// KYC module's transaction (kyc.Hooks.OnSubmitted).
func (s *Service) KYCSubmitted(ctx context.Context, tx pgx.Tx, ev KYCEvent) error {
	return s.inTx(ctx, tx, func(t *txn) error {
		u := t.userForUpdate(ev.UserID)
		if u == nil {
			return domain.Fail("Akun tidak ditemukan.")
		}
		if u.Verification.Status == "terverifikasi" {
			return domain.Fail("Akunmu sudah terverifikasi.")
		}
		id := domain.NewID("v")
		t.check(t.q.InsertVerification(t.ctx, dbq.InsertVerificationParams{ID: id, UserID: u.ID, Method: "manual", NameOnKtp: "",
			NikMasked: ev.NIKMasked, Status: "menunggu", Auto: false, SubmittedAt: tsOf(t.now)}))
		_, err := t.tx.Exec(t.ctx, `UPDATE verification_requests SET kyc_submission_id = $2 WHERE id = $1`, id, ev.SubmissionID)
		t.check(err)
		u.Verification = domain.Verification{Status: "menunggu", Method: domain.Ptr("manual")}
		t.saveUser(u)
		t.notify(u.ID, "verifikasi", "Verifikasi sedang ditinjau", "KTP dan selfie-mu sudah kami terima. Admin meninjau paling lambat 1×24 jam.", "/verifikasi")
		t.notify(domain.AdminID, "verifikasi", "Pengajuan verifikasi baru", domain.PublicName(u)+" mengunggah KTP + selfie.", "/admin/verifikasi")
		if ev.Status != "pending" {
			return t.applyKYCDecision(ev)
		}
		return nil
	})
}

// KYCDecided mirrors an approved/rejected KYC submission (kyc.Hooks.OnDecided): verification request reviewed, user
// "terverifikasi" with the masked NIK, or "ditolak" with the reason.
func (s *Service) KYCDecided(ctx context.Context, tx pgx.Tx, ev KYCEvent) error {
	return s.inTx(ctx, tx, func(t *txn) error { return t.applyKYCDecision(ev) })
}

func (t *txn) applyKYCDecision(ev KYCEvent) error {
	approve := ev.Status == "approved"
	var reqID string
	err := t.tx.QueryRow(t.ctx, `SELECT id FROM verification_requests WHERE kyc_submission_id = $1 ORDER BY ord DESC LIMIT 1 FOR UPDATE`,
		ev.SubmissionID).Scan(&reqID)
	if !t.found(err) {
		if t.err != nil {
			return nil
		}
		// Submitted before the hooks were wired: create the mirror row now.
		reqID = domain.NewID("v")
		t.check(t.q.InsertVerification(t.ctx, dbq.InsertVerificationParams{ID: reqID, UserID: ev.UserID, Method: "manual", NikMasked: ev.NIKMasked,
			Status: "menunggu", SubmittedAt: tsOf(t.now)}))
		_, err = t.tx.Exec(t.ctx, `UPDATE verification_requests SET kyc_submission_id = $2 WHERE id = $1`, reqID, ev.SubmissionID)
		t.check(err)
	}
	status, reason := "ditolak", domain.Ptr(ev.Reason)
	if approve {
		status = "disetujui"
		if ev.Reason == "" {
			reason = nil
		}
	}
	reviewedAt := t.now
	if ev.DecidedAt != nil {
		reviewedAt = msOf(*ev.DecidedAt)
	}
	t.check(t.q.UpdateVerificationReview(t.ctx, dbq.UpdateVerificationReviewParams{ID: reqID, Status: status, ReviewedAt: tsPtr(&reviewedAt),
		ReviewedBy: domain.Ptr(ev.DecidedBy), Reason: reason}))
	u := t.userForUpdate(ev.UserID)
	if u == nil {
		return nil
	}
	u.Verification.Method = domain.Ptr("manual")
	if approve {
		u.Verification.Status, u.Verification.VerifiedAt, u.Verification.NikMasked = "terverifikasi", domain.Ptr(reviewedAt), domain.Ptr(ev.NIKMasked)
		t.notify(u.ID, "verifikasi", "Identitas terverifikasi", "Akunmu sudah terverifikasi oleh admin.", "/profil")
	} else {
		u.Verification.Status, u.Verification.VerifiedAt = "ditolak", nil
		t.notify(u.ID, "verifikasi", "Verifikasi ditolak", "Alasan: "+domain.Deref(reason, "-")+". Silakan ulangi verifikasi.", "/verifikasi")
	}
	t.saveUser(u)
	action := "verifikasi.tolak"
	if approve {
		action = "verifikasi.setuju"
	}
	t.audit(ev.DecidedBy, action, domain.PublicName(u), nil, reason)
	return nil
}

// reviewViaKYC handles reviewVerification for a request that came from a real KYC upload: the decision goes through
// the KYC module (kyc.Decide, which creates the identity, audits and schedules the file purge) and its hook updates
// the core rows. handled=false means the request is a demo/simulated one and the normal action runs.
func (s *Service) reviewViaKYC(ctx context.Context, viewer *Viewer, name string, args []json.RawMessage) (Result, bool, error) {
	if name != "reviewVerification" || len(args) < 2 {
		return Result{}, false, nil
	}
	var adminID, requestID string
	var approve bool
	var reason *string
	_ = json.Unmarshal(args[0], &adminID)
	_ = json.Unmarshal(args[1], &requestID)
	if len(args) > 2 {
		_ = json.Unmarshal(args[2], &approve)
	}
	if len(args) > 3 {
		_ = json.Unmarshal(args[3], &reason)
	}
	var subID *string
	err := s.pool.QueryRow(ctx, `SELECT kyc_submission_id FROM verification_requests WHERE id = $1`, requestID).Scan(&subID)
	if isNoRows(err) || (err == nil && subID == nil) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, true, err
	}
	if viewer == nil || viewer.UserID != adminID {
		return Result{}, true, domain.ErrForbidden
	}
	if !viewer.IsAdmin() {
		return Result{}, true, domain.Fail("Hanya admin.")
	}
	s.mu.RLock()
	decide := s.kycDecider
	s.mu.RUnlock()
	if decide == nil {
		return Result{}, true, domain.Fail("Pengajuan ini diputuskan lewat antrean KYC.")
	}
	if err := decide(ctx, adminID, *subID, approve, domain.Deref(reason, "")); err != nil {
		return Result{}, true, err
	}
	v, err := s.Version(ctx)
	return Result{Version: v}, true, err
}

// blacklistIdentity adds the user's NIK (verified identity and any KYC submission) to nik_blacklist and revokes
// every session, in the action's transaction (FR-A3, PRD §11.2 L3).
func (t *txn) blacklistIdentity(adminID, userID, reason string) {
	_, err := t.tx.Exec(t.ctx, `
		INSERT INTO nik_blacklist (nik_hmac, reason, created_by, created_at)
		SELECT DISTINCT nik_hmac, $2::text, $3::text, $4::timestamptz FROM (
		  SELECT nik_hmac FROM identities WHERE user_id = $1
		  UNION SELECT nik_hmac FROM kyc_submissions WHERE user_id = $1) x
		ON CONFLICT (nik_hmac) DO UPDATE SET reason = EXCLUDED.reason`, userID, reason, adminID, tsOf(t.now))
	t.check(err)
	_, err = t.tx.Exec(t.ctx, `UPDATE auth_sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, tsOf(t.now))
	t.check(err)
}
