package kyc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mrfroglab/sewain-api/internal/auth"
	"github.com/mrfroglab/sewain-api/internal/crypto"
)

func userErr(status int, msg string) *auth.Error { return &auth.Error{Status: status, Msg: msg} }

var (
	errVerified        = userErr(http.StatusConflict, "Akunmu sudah terverifikasi.")
	errPendingExists   = userErr(http.StatusConflict, "Pengajuan verifikasimu masih diproses admin. Tunggu kabar dari kami ya.")
	errNIKTaken        = userErr(http.StatusConflict, "NIK ini sudah dipakai akun lain. Satu NIK hanya untuk satu akun.")
	errNIKBlacklisted  = userErr(http.StatusForbidden, "NIK ini tidak bisa dipakai untuk verifikasi. Hubungi admin kalau menurutmu ini keliru.")
	errNameRequired    = userErr(http.StatusBadRequest, "Tulis nama sesuai KTP.")
	errFileMissing     = userErr(http.StatusBadRequest, "Foto KTP dan selfie wajib diunggah.")
	errFileTooBig      = userErr(http.StatusRequestEntityTooLarge, "Ukuran foto maksimal 8 MB.")
	errFileType        = userErr(http.StatusUnsupportedMediaType, "Foto harus berformat JPG, PNG, atau WebP.")
	errNotFound        = userErr(http.StatusNotFound, "Pengajuan verifikasi tidak ditemukan.")
	errAlreadyDecided  = userErr(http.StatusConflict, "Pengajuan ini sudah diputuskan.")
	errReasonRequired  = userErr(http.StatusBadRequest, "Tulis alasan penolakan.")
	errBadDecision     = userErr(http.StatusBadRequest, "Keputusan harus approve atau reject.")
	errBadKind         = userErr(http.StatusNotFound, "Jenis berkas tidak dikenal.")
	errFilesPurged     = userErr(http.StatusGone, "Berkas sudah dihapus sesuai kebijakan retensi.")
	errAuditFailed     = userErr(http.StatusServiceUnavailable, "Akses berkas ditolak karena audit log sedang bermasalah. Coba lagi nanti.")
	errBlacklistTarget = userErr(http.StatusNotFound, "Pengguna ini belum punya NIK terdaftar.")
)

// SubmitInput is a KYC submission from a user.
type SubmitInput struct {
	NIK       string
	NameOnKTP string
	KTP       File
	Selfie    File
}

var allowedTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

func checkFile(f File) (File, error) {
	if len(f.Data) == 0 {
		return f, errFileMissing
	}
	if len(f.Data) > MaxFileBytes {
		return f, errFileTooBig
	}
	// Trust the bytes, not the client's Content-Type.
	ct := http.DetectContentType(f.Data)
	if !allowedTypes[ct] {
		return f, errFileType
	}
	f.ContentType = ct
	return f, nil
}

func normalizeName(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) < 2 || utf8.RuneCountInString(s) > 100 {
		return "", errNameRequired
	}
	return s, nil
}

func lockKey(tx pgx.Tx, ctx context.Context, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key)
	return err
}

// sealFile encrypts contentType NUL data, so the content type survives without a plaintext column.
func (s *Service) sealFile(id, kind string, f File) ([]byte, error) {
	pt := make([]byte, 0, len(f.ContentType)+1+len(f.Data))
	pt = append(pt, f.ContentType...)
	pt = append(pt, 0)
	pt = append(pt, f.Data...)
	return s.Config.Keys.Seal(pt, aad("file:"+kind, id))
}

func (s *Service) openFile(id, kind string, blob []byte) ([]byte, string, error) {
	pt, err := s.Config.Keys.Open(blob, aad("file:"+kind, id))
	if err != nil {
		return nil, "", err
	}
	i := bytes.IndexByte(pt, 0)
	if i < 0 {
		return nil, "", errors.New("kyc: malformed file envelope")
	}
	return pt[i+1:], string(pt[:i]), nil
}

// Submit validates, encrypts and stores a submission, then asks the provider for a verdict (manual: queue).
func (s *Service) Submit(ctx context.Context, userID string, in SubmitInput) (Submission, error) {
	nik := strings.TrimSpace(in.NIK)
	if err := ValidateNIK(nik); err != nil {
		return Submission{}, err
	}
	name, err := normalizeName(in.NameOnKTP)
	if err != nil {
		return Submission{}, err
	}
	ktp, err := checkFile(in.KTP)
	if err != nil {
		return Submission{}, err
	}
	selfie, err := checkFile(in.Selfie)
	if err != nil {
		return Submission{}, err
	}
	hmac := s.nikHMAC(nik)
	idb, err := crypto.RandomBytes(12)
	if err != nil {
		return Submission{}, err
	}
	id := "kyc-" + hex.EncodeToString(idb)
	now := s.now()

	nikEnc, err := s.Config.Keys.Seal([]byte(nik), aad("nik", id))
	if err != nil {
		return Submission{}, err
	}
	nameEnc, err := s.Config.Keys.Seal([]byte(name), aad("name", id))
	if err != nil {
		return Submission{}, err
	}
	ktpEnc, err := s.sealFile(id, FileKTP, ktp)
	if err != nil {
		return Submission{}, err
	}
	selfieEnc, err := s.sealFile(id, FileSelfie, selfie)
	if err != nil {
		return Submission{}, err
	}

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Submission{}, err
	}
	defer tx.Rollback(ctx)
	// Lock order: user, then NIK (same as Decide) so concurrent submissions cannot race past the checks.
	if err := lockKey(tx, ctx, "kyc:user:"+userID); err != nil {
		return Submission{}, err
	}
	if err := lockKey(tx, ctx, "kyc:nik:"+hex.EncodeToString(hmac)); err != nil {
		return Submission{}, err
	}
	if err := s.checkNIKAvailable(ctx, tx, userID, hmac, true); err != nil {
		return Submission{}, err
	}

	sub := Submission{ID: id, UserID: userID, NIKMasked: MaskNIK(nik), Status: StatusPending,
		Provider: s.Provider.Name(), CreatedAt: now}
	keys := map[string][]byte{objectKey(id, FileKTP): ktpEnc, objectKey(id, FileSelfie): selfieEnc}
	committed := false
	defer func() {
		if !committed {
			for k := range keys {
				if err := s.Blobs.Delete(context.WithoutCancel(ctx), k); err != nil {
					log.Printf("kyc: cleanup blob %s: %v", k, err)
				}
			}
		}
	}()
	for k, data := range keys {
		if err := s.Blobs.Put(ctx, k, data, "application/octet-stream"); err != nil {
			return Submission{}, err
		}
	}
	verdict, err := s.Provider.Verify(ctx, VerifyRequest{SubmissionID: id, UserID: userID, NIK: nik, NameOnKTP: name, KTP: ktp, Selfie: selfie})
	if err != nil {
		log.Printf("kyc: provider %s failed for %s, falling back to manual review: %v", s.Provider.Name(), id, err)
		verdict = VerifyResult{Decision: StatusPending}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO kyc_submissions (id, user_id, nik_hmac, nik_enc, nik_masked, name_enc, ktp_object, selfie_object,
			provider, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', $10, $10)`,
		id, userID, hmac, nikEnc, sub.NIKMasked, nameEnc, objectKey(id, FileKTP), objectKey(id, FileSelfie),
		sub.Provider, now); err != nil {
		return Submission{}, err
	}
	if s.Hooks.OnSubmitted != nil {
		if err := s.Hooks.OnSubmitted(ctx, tx, sub); err != nil {
			return Submission{}, err
		}
	}
	switch verdict.Decision {
	case StatusApproved, StatusRejected:
		reason := verdict.Reason
		if verdict.Decision == StatusRejected && reason == "" {
			reason = "Verifikasi otomatis gagal."
		}
		sub, err = s.decideTx(ctx, tx, "system:"+s.Provider.Name(), id, verdict.Decision, reason)
		if err != nil {
			return Submission{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Submission{}, err
	}
	committed = true
	return sub, nil
}

// checkNIKAvailable enforces the blacklist and one NIK per account. forSubmit adds the per-user checks.
func (s *Service) checkNIKAvailable(ctx context.Context, tx pgx.Tx, userID string, hmac []byte, forSubmit bool) error {
	var blacklisted, verified, pendingMine, takenByOther bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM nik_blacklist WHERE nik_hmac = $2),
		       EXISTS (SELECT 1 FROM identities WHERE user_id = $1),
		       EXISTS (SELECT 1 FROM kyc_submissions WHERE user_id = $1 AND status = 'pending'),
		       EXISTS (SELECT 1 FROM identities WHERE nik_hmac = $2 AND user_id <> $1)
		    OR EXISTS (SELECT 1 FROM kyc_submissions WHERE nik_hmac = $2 AND user_id <> $1 AND status = 'pending')`,
		userID, hmac).Scan(&blacklisted, &verified, &pendingMine, &takenByOther)
	if err != nil {
		return err
	}
	switch {
	case blacklisted:
		return errNIKBlacklisted
	case forSubmit && verified:
		return errVerified
	case forSubmit && pendingMine:
		return errPendingExists
	case takenByOther:
		return errNIKTaken
	}
	return nil
}

const submissionCols = `id, user_id, nik_masked, name_enc, status, reason, provider, created_at, decided_at,
	coalesce(decided_by, ''), files_purge_after, files_purged_at`

func (s *Service) scanSubmission(row pgx.Row, withName bool) (Submission, error) {
	var x Submission
	var nameEnc []byte
	err := row.Scan(&x.ID, &x.UserID, &x.NIKMasked, &nameEnc, &x.Status, &x.Reason, &x.Provider, &x.CreatedAt,
		&x.DecidedAt, &x.DecidedBy, &x.FilesPurgeAfter, &x.FilesPurgedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, errNotFound
	}
	if err != nil {
		return x, err
	}
	if withName {
		name, err := s.Config.Keys.Open(nameEnc, aad("name", x.ID))
		if err != nil {
			return x, fmt.Errorf("kyc: decrypt name of %s: %w", x.ID, err)
		}
		x.NameOnKTP = string(name)
	}
	return x, nil
}

// Status returns the user's latest submission (without the decrypted name), or found=false.
func (s *Service) Status(ctx context.Context, userID string) (Submission, bool, error) {
	x, err := s.scanSubmission(s.DB.QueryRow(ctx, `SELECT `+submissionCols+` FROM kyc_submissions
		WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1`, userID), false)
	if errors.Is(err, errNotFound) {
		return Submission{}, false, nil
	}
	return x, err == nil, err
}

// Queue lists submissions for admins (status "" = all), oldest first, with decrypted names.
func (s *Service) Queue(ctx context.Context, v auth.Viewer, status string, limit int) ([]Submission, error) {
	if err := auth.CheckAdmin(v); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.Query(ctx, `SELECT `+submissionCols+` FROM kyc_submissions
		WHERE ($1 = '' OR status = $1) ORDER BY created_at ASC LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Submission{}
	for rows.Next() {
		x, err := s.scanSubmission(rows, true)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Get returns one submission for admins, with the decrypted name.
func (s *Service) Get(ctx context.Context, v auth.Viewer, id string) (Submission, error) {
	if err := auth.CheckAdmin(v); err != nil {
		return Submission{}, err
	}
	return s.scanSubmission(s.DB.QueryRow(ctx, `SELECT `+submissionCols+` FROM kyc_submissions WHERE id = $1`, id), true)
}

// Decide approves or rejects a pending submission (decision "approve"|"reject"). Rejections need a reason.
// Approval creates the verified identity (one NIK per account). Files are scheduled for purge
// FileRetention after the decision.
func (s *Service) Decide(ctx context.Context, v auth.Viewer, id, decision, reason string) (Submission, error) {
	if err := auth.CheckAdmin(v); err != nil {
		return Submission{}, err
	}
	var status string
	switch decision {
	case "approve", StatusApproved:
		status = StatusApproved
	case "reject", StatusRejected:
		status = StatusRejected
	default:
		return Submission{}, errBadDecision
	}
	reason = strings.TrimSpace(reason)
	if status == StatusRejected && reason == "" {
		return Submission{}, errReasonRequired
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return Submission{}, err
	}
	defer tx.Rollback(ctx)
	var userID string
	if err := tx.QueryRow(ctx, `SELECT user_id FROM kyc_submissions WHERE id = $1`, id).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Submission{}, errNotFound
		}
		return Submission{}, err
	}
	if err := lockKey(tx, ctx, "kyc:user:"+userID); err != nil {
		return Submission{}, err
	}
	sub, err := s.decideTx(ctx, tx, v.UserID, id, status, reason)
	if err != nil {
		return Submission{}, err
	}
	return sub, tx.Commit(ctx)
}

func (s *Service) decideTx(ctx context.Context, tx pgx.Tx, actorID, id, status, reason string) (Submission, error) {
	var userID, curStatus, masked string
	var hmac, nikEnc []byte
	err := tx.QueryRow(ctx, `SELECT user_id, status, nik_hmac, nik_enc, nik_masked FROM kyc_submissions WHERE id = $1 FOR UPDATE`, id).
		Scan(&userID, &curStatus, &hmac, &nikEnc, &masked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Submission{}, errNotFound
	}
	if err != nil {
		return Submission{}, err
	}
	if curStatus != StatusPending {
		return Submission{}, errAlreadyDecided
	}
	now := s.now()
	if status == StatusApproved {
		if err := lockKey(tx, ctx, "kyc:nik:"+hex.EncodeToString(hmac)); err != nil {
			return Submission{}, err
		}
		if err := s.checkNIKAvailable(ctx, tx, userID, hmac, false); err != nil {
			return Submission{}, err
		}
		// Re-seal the NIK for the identity row, bound to the user instead of the submission, so it outlives
		// the submission row.
		nik, err := s.Config.Keys.Open(nikEnc, aad("nik", id))
		if err != nil {
			return Submission{}, err
		}
		idEnc, err := s.Config.Keys.Seal(nik, aad("identity-nik", userID))
		if err != nil {
			return Submission{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identities (user_id, nik_hmac, nik_enc, nik_masked, submission_id, verified_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6, $6)
			ON CONFLICT (user_id) DO UPDATE SET nik_hmac = EXCLUDED.nik_hmac, nik_enc = EXCLUDED.nik_enc,
				nik_masked = EXCLUDED.nik_masked, submission_id = EXCLUDED.submission_id,
				verified_at = EXCLUDED.verified_at, updated_at = EXCLUDED.updated_at`,
			userID, hmac, idEnc, masked, id, now); err != nil {
			return Submission{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE kyc_submissions SET status = $2, reason = $3, decided_at = $4, decided_by = $5,
			files_purge_after = $6, updated_at = $4 WHERE id = $1`,
		id, status, reason, now, actorID, now.Add(FileRetention)); err != nil {
		return Submission{}, err
	}
	sub, err := s.scanSubmission(tx.QueryRow(ctx, `SELECT `+submissionCols+` FROM kyc_submissions WHERE id = $1`, id), false)
	if err != nil {
		return Submission{}, err
	}
	old, nv := StatusPending, status
	if err := s.Auditor.Audit(ctx, actorID, "kyc."+strings.TrimSuffix(status, "d"), "kyc_submission:"+id, &old, &nv); err != nil {
		return Submission{}, fmt.Errorf("kyc: audit decision: %w", err)
	}
	if s.Hooks.OnDecided != nil {
		if err := s.Hooks.OnDecided(ctx, tx, sub); err != nil {
			return Submission{}, err
		}
	}
	return sub, nil
}

// ReadFile returns a decrypted KTP or selfie for an admin. Every attempt is written to the audit log first;
// if auditing fails, access is refused.
func (s *Service) ReadFile(ctx context.Context, v auth.Viewer, id, kind string) ([]byte, string, error) {
	if err := auth.CheckAdmin(v); err != nil {
		return nil, "", err
	}
	// Audit every attempt, including unknown IDs and kinds, before touching any data.
	target := "kyc_submission:" + truncate(id, 64) + "/" + truncate(kind, 16)
	if err := s.Auditor.Audit(ctx, v.UserID, "kyc.file.read", target, nil, nil); err != nil {
		log.Printf("kyc: audit file read %s by %s: %v", target, v.UserID, err)
		return nil, "", errAuditFailed
	}
	if kind != FileKTP && kind != FileSelfie {
		return nil, "", errBadKind
	}
	var obj *string
	var purgedAt *time.Time
	col := "ktp_object"
	if kind == FileSelfie {
		col = "selfie_object"
	}
	err := s.DB.QueryRow(ctx, `SELECT `+col+`, files_purged_at FROM kyc_submissions WHERE id = $1`, id).Scan(&obj, &purgedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", errNotFound
	}
	if err != nil {
		return nil, "", err
	}
	if purgedAt != nil || obj == nil {
		return nil, "", errFilesPurged
	}
	blob, _, err := s.Blobs.Get(ctx, *obj)
	if err != nil {
		return nil, "", fmt.Errorf("kyc: get %s: %w", *obj, err)
	}
	return s.openFile(id, kind, blob)
}

// PurgeFiles deletes the files of submissions whose retention ended (files_purge_after <= now) and marks
// them purged. The verification result (status, masked NIK, identity) is kept. Safe to run on several
// instances concurrently (SKIP LOCKED). A submission whose files cannot be deleted is skipped for the rest
// of the run, so it never blocks the others; its error is returned (joined) with the number purged.
func (s *Service) PurgeFiles(ctx context.Context, now time.Time) (int, error) {
	now = now.UTC().Truncate(time.Microsecond)
	purged := 0
	skip := []string{}
	var errs []error
	for {
		id, ok, err := s.purgeOne(ctx, now, skip)
		switch {
		case err != nil && id == "":
			return purged, errors.Join(append(errs, err)...)
		case err != nil:
			errs = append(errs, err)
			skip = append(skip, id)
		case !ok:
			return purged, errors.Join(errs...)
		default:
			purged++
		}
	}
}

// purgeOne purges the oldest due submission not in skip. It returns the submission it worked on (if any).
func (s *Service) purgeOne(ctx context.Context, now time.Time, skip []string) (string, bool, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)
	var id string
	var ktp, selfie *string
	err = tx.QueryRow(ctx, `
		SELECT id, ktp_object, selfie_object FROM kyc_submissions
		WHERE files_purged_at IS NULL AND files_purge_after <= $1 AND NOT (id = ANY($2))
		ORDER BY files_purge_after LIMIT 1 FOR UPDATE SKIP LOCKED`, now, skip).Scan(&id, &ktp, &selfie)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, k := range []*string{ktp, selfie} {
		if k == nil {
			continue
		}
		if err := s.Blobs.Delete(ctx, *k); err != nil {
			if ok, exErr := s.Blobs.Exists(ctx, *k); exErr != nil || ok {
				return id, false, fmt.Errorf("kyc: purge %s: %w", *k, err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE kyc_submissions SET files_purged_at = $2, ktp_object = NULL, selfie_object = NULL, updated_at = $2
		WHERE id = $1`, id, now); err != nil {
		return id, false, err
	}
	if err := s.Auditor.Audit(ctx, "system", "kyc.files.purge", "kyc_submission:"+id, nil, nil); err != nil {
		return id, false, err
	}
	return id, true, tx.Commit(ctx)
}

// EraseUser removes a user's identity data for account deletion (UU PDP): the encrypted KTP/selfie objects
// of every submission, the submissions and the verified identity. The NIK blacklist is kept (it holds only
// an HMAC and is what stops a blacklisted person from re-registering). Files are deleted before the rows,
// so a failure leaves everything in place for a retry. Idempotent; returns the number of submissions
// erased. Call it from the core's account deletion, after revoking sessions.
func (s *Service) EraseUser(ctx context.Context, userID string) (int, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockKey(tx, ctx, "kyc:user:"+userID); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id, ktp_object, selfie_object FROM kyc_submissions WHERE user_id = $1 FOR UPDATE`, userID)
	if err != nil {
		return 0, err
	}
	var keys []string
	n := 0
	for rows.Next() {
		var id string
		var ktp, selfie *string
		if err := rows.Scan(&id, &ktp, &selfie); err != nil {
			rows.Close()
			return 0, err
		}
		n++
		// Derive the keys too, in case a row lost its object column but the file is still there.
		keys = append(keys, objectKey(id, FileKTP), objectKey(id, FileSelfie))
		for _, k := range []*string{ktp, selfie} {
			if k != nil && *k != objectKey(id, FileKTP) && *k != objectKey(id, FileSelfie) {
				keys = append(keys, *k)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, k := range keys {
		if err := s.Blobs.Delete(ctx, k); err != nil {
			if ok, exErr := s.Blobs.Exists(ctx, k); exErr != nil || ok {
				return 0, fmt.Errorf("kyc: erase %s: %w", k, err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE user_id = $1`, userID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM kyc_submissions WHERE user_id = $1`, userID); err != nil {
		return 0, err
	}
	if n > 0 {
		if err := s.Auditor.Audit(ctx, "system", "kyc.erase", "user:"+userID, nil, nil); err != nil {
			return 0, fmt.Errorf("kyc: audit erase: %w", err)
		}
	}
	return n, tx.Commit(ctx)
}

// BlacklistNIK adds a NIK to the blacklist (admin). Future submissions and approvals with it are refused.
func (s *Service) BlacklistNIK(ctx context.Context, v auth.Viewer, nik, reason string) error {
	if err := auth.CheckAdmin(v); err != nil {
		return err
	}
	nik = strings.TrimSpace(nik)
	if err := ValidateNIK(nik); err != nil {
		return err
	}
	return s.blacklist(ctx, v.UserID, s.nikHMAC(nik), "nik:"+MaskNIK(nik), reason)
}

// BlacklistUser blacklists the NIK of a user's verified identity (or latest submission) without the admin
// ever seeing the NIK. Setting users.status is the core's job.
func (s *Service) BlacklistUser(ctx context.Context, v auth.Viewer, userID, reason string) error {
	if err := auth.CheckAdmin(v); err != nil {
		return err
	}
	var hmac []byte
	err := s.DB.QueryRow(ctx, `
		SELECT nik_hmac FROM (
			SELECT nik_hmac, 0 AS pri, verified_at AS at FROM identities WHERE user_id = $1
			UNION ALL
			SELECT nik_hmac, 1, created_at FROM kyc_submissions WHERE user_id = $1
		) x ORDER BY pri, at DESC LIMIT 1`, userID).Scan(&hmac)
	if errors.Is(err, pgx.ErrNoRows) {
		return errBlacklistTarget
	}
	if err != nil {
		return err
	}
	return s.blacklist(ctx, v.UserID, hmac, "user:"+userID, reason)
}

func (s *Service) blacklist(ctx context.Context, actorID string, hmac []byte, target, reason string) error {
	reason = strings.TrimSpace(reason)
	if _, err := s.DB.Exec(ctx, `
		INSERT INTO nik_blacklist (nik_hmac, reason, created_by, created_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (nik_hmac) DO UPDATE SET reason = EXCLUDED.reason`, hmac, reason, actorID, s.now()); err != nil {
		return err
	}
	return s.Auditor.Audit(ctx, actorID, "kyc.blacklist", target, nil, &reason)
}

// IsBlacklistedUser reports whether the user's verified NIK is on the blacklist (e.g. to block re-registration
// flows in core).
func (s *Service) IsBlacklistedUser(ctx context.Context, userID string) (bool, error) {
	var b bool
	err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM identities i JOIN nik_blacklist b USING (nik_hmac) WHERE i.user_id = $1)`, userID).Scan(&b)
	return b, err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
