package service

import (
	"strings"

	"github.com/mrfroglab/sewain-api/internal/db"
	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

func (t *txn) registerUser(in domain.RegisterInput, wantID string) (domain.ID, error) {
	if !t.s.opt.DemoMode {
		// Outside the demo accounts are created by OTP verification (POST /api/v1/auth/otp/verify).
		return "", domain.Fail("Daftar pakai nomor HP dan kode OTP, ya.")
	}
	phone := domain.Digits(in.Phone)
	if len(phone) < 9 {
		return "", domain.Fail("Nomor HP tidak valid.")
	}
	if strings.TrimSpace(in.Name) == "" {
		return "", domain.Fail("Nama wajib diisi.")
	}
	if err := domain.FirstErr(domain.CheckLen("Nama", strings.TrimSpace(in.Name), domain.MaxNameLen), checkStore(in.Store),
		domain.CheckLenPtr("Email", in.Email, maxEmailLen)); err != nil {
		return "", err
	}
	taken, err := t.q.PhoneTaken(t.ctx, phone)
	t.check(err)
	if taken {
		return "", domain.Fail("Nomor HP sudah terdaftar. Masuk pakai nomor itu.")
	}
	if in.AccountType != "toko" {
		in.AccountType = "individu"
	}
	if _, ok := domain.Areas[in.AreaID]; !ok {
		in.AreaID = "ugm"
	}
	id := t.pickID(wantID, "u", t.q.UserExists)
	u := &domain.User{ID: id, Name: strings.TrimSpace(in.Name), Phone: phone, Email: in.Email, Role: "user", AccountType: in.AccountType, Store: in.Store,
		Verification: domain.Verification{Status: "belum"}, AreaID: in.AreaID, JoinedAt: t.now, Status: "aktif"}
	if err := t.insertUser(u); err != nil {
		return "", err
	}
	t.notify(id, "verifikasi", "Selamat datang di Sewain!", "Verifikasi KTP + selfie dulu supaya bisa menyewa dan menyewakan.", "/verifikasi")
	return id, nil
}

// insertUser creates a user (+ store). A duplicate e-mail is a rule violation, not a server error.
func (t *txn) insertUser(u *domain.User) error {
	if _, err := t.tx.Exec(t.ctx, "SAVEPOINT insert_user"); !t.check(err) {
		return nil
	}
	err := t.q.InsertUser(t.ctx, insertUserParams(u))
	if db.IsUniqueViolation(err, "users_email_key") {
		_, rerr := t.tx.Exec(t.ctx, "ROLLBACK TO SAVEPOINT insert_user")
		t.check(rerr)
		return domain.Fail("Email sudah dipakai akun lain.")
	}
	if db.IsUniqueViolation(err, "users_phone_key") {
		_, rerr := t.tx.Exec(t.ctx, "ROLLBACK TO SAVEPOINT insert_user")
		t.check(rerr)
		return domain.Fail("Nomor HP sudah terdaftar. Masuk pakai nomor itu.")
	}
	t.check(err)
	if u.Store != nil {
		t.saveStore(u)
	}
	t.touch(u.ID)
	t.touchPublic()
	return nil
}

func (t *txn) updateUser(userID domain.ID, p domain.UserPatch) error {
	if err := checkUserPatch(p); err != nil {
		return err
	}
	u := t.userForUpdate(userID)
	if u == nil {
		return domain.Fail("Akun tidak ditemukan.")
	}
	if p.Name != nil && strings.TrimSpace(*p.Name) != "" {
		u.Name = strings.TrimSpace(*p.Name)
	}
	if p.Nickname != nil {
		u.Nickname = p.Nickname
	}
	if p.Email != nil {
		u.Email = p.Email
	}
	if p.Bio != nil {
		u.Bio = p.Bio
	}
	if p.AreaID != nil {
		if _, ok := domain.Areas[*p.AreaID]; ok {
			u.AreaID = *p.AreaID
		}
	}
	if p.AccountType != nil && (*p.AccountType == "individu" || *p.AccountType == "toko") {
		u.AccountType = *p.AccountType
	}
	if p.Store != nil {
		u.Store = p.Store
	}
	if p.Campus != nil {
		u.Campus = p.Campus
	}
	if _, err := t.tx.Exec(t.ctx, "SAVEPOINT update_user"); !t.check(err) {
		return nil
	}
	err := t.q.UpdateUser(t.ctx, updateUserParams(u))
	if db.IsUniqueViolation(err, "users_email_key") {
		_, rerr := t.tx.Exec(t.ctx, "ROLLBACK TO SAVEPOINT update_user")
		t.check(rerr)
		return domain.Fail("Email sudah dipakai akun lain.")
	}
	t.check(err)
	t.saveStore(u)
	t.touch(u.ID)
	t.touchPublic()
	return nil
}

// submitVerification: prototype eKYC is simulated and approves instantly (PRD §7.2), but the rules are real:
// one NIK = one account (FR-A3) and blacklisted NIKs cannot come back.
func (t *txn) submitVerification(userID domain.ID, in domain.VerificationInput, wantID string) (domain.ID, error) {
	if !t.s.opt.DemoMode {
		// Production: KTP + selfie are uploaded encrypted (POST /api/v1/kyc/submissions) and reviewed by an admin.
		return "", domain.Fail("Unggah foto KTP dan selfie di halaman verifikasi, ya.")
	}
	u := t.userForUpdate(userID)
	if u == nil {
		return "", domain.Fail("Akun tidak ditemukan.")
	}
	if err := domain.CheckLen("Nama di KTP", in.NameOnKtp, domain.MaxNameLen); err != nil {
		return "", err
	}
	nik := domain.Digits(in.Nik)
	masked := domain.MaskNik(nik)
	if len(nik) >= 8 {
		other, err := t.q.OtherUserByNikMasked(t.ctx, dbq.OtherUserByNikMaskedParams{NikMasked: &masked, UserID: userID})
		if t.found(err) {
			if other.Status == "blacklist" {
				return "", domain.Fail("NIK ini diblokir dari Sewain.")
			}
			return "", domain.Fail("NIK ini sudah dipakai akun lain. Satu NIK hanya untuk satu akun.")
		}
	}
	method := in.Method
	if method != "ekyc" && method != "demo" && method != "manual" {
		method = "demo"
	}
	id := t.pickID(wantID, "v", t.q.VerificationExists)
	t.check(t.q.InsertVerification(t.ctx, dbq.InsertVerificationParams{ID: id, UserID: userID, Method: method, NameOnKtp: strings.ToUpper(in.NameOnKtp),
		NikMasked: masked, KtpPhotoID: in.KtpPhotoID, SelfiePhotoID: in.SelfiePhotoID, Status: "disetujui", Auto: true,
		SubmittedAt: tsOf(t.now), ReviewedAt: tsPtr(&t.now)}))
	u.Verification = domain.Verification{Status: "terverifikasi", Method: domain.Ptr(method), VerifiedAt: domain.Ptr(t.now), NikMasked: domain.Ptr(masked)}
	t.saveUser(u)
	t.notify(userID, "verifikasi", "Identitas terverifikasi", "Sekarang kamu bisa menyewa dan menyewakan barang.", "/profil")
	return id, nil
}

func (t *txn) reviewVerification(adminID, requestID domain.ID, approve bool, reason *string) error {
	if !t.isAdmin(adminID) {
		return domain.Fail("Hanya admin.")
	}
	r, err := t.q.GetVerificationForUpdate(t.ctx, requestID)
	if !t.found(err) {
		return domain.Fail("Pengajuan tidak ditemukan.")
	}
	req := verificationFromRow(r)
	if req.Status != "menunggu" {
		return domain.Fail("Pengajuan ini sudah diputuskan (" + req.Status + ").")
	}
	if err := domain.CheckLenPtr("Alasan", reason, domain.MaxReasonLen); err != nil {
		return err
	}
	req.Status, req.ReviewedAt, req.ReviewedBy, req.Reason = "ditolak", domain.Ptr(t.now), domain.Ptr(adminID), reason
	if approve {
		req.Status = "disetujui"
	}
	t.check(t.q.UpdateVerificationReview(t.ctx, dbq.UpdateVerificationReviewParams{ID: req.ID, Status: req.Status, ReviewedAt: tsPtr(req.ReviewedAt),
		ReviewedBy: req.ReviewedBy, Reason: req.Reason}))
	t.touch(req.UserID)
	if u := t.userForUpdate(req.UserID); u != nil {
		u.Verification.Method = domain.Ptr("manual")
		u.Verification.NikMasked = domain.Ptr(req.NikMasked)
		if approve {
			u.Verification.Status, u.Verification.VerifiedAt = "terverifikasi", domain.Ptr(t.now)
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
		t.audit(adminID, action, domain.PublicName(u), nil, reason)
	}
	return nil
}

// maxEmailLen is the longest valid e-mail address (RFC 5321 path limit).
const maxEmailLen = 254

// checkUserPatch bounds the profile texts (review wave 2, L2).
func checkUserPatch(p domain.UserPatch) error {
	trim := func(s *string) *string {
		if s == nil {
			return nil
		}
		return domain.Ptr(strings.TrimSpace(*s))
	}
	return domain.FirstErr(
		domain.CheckLenPtr("Nama", trim(p.Name), domain.MaxNameLen),
		domain.CheckLenPtr("Nama panggilan", p.Nickname, domain.MaxNameLen),
		domain.CheckLenPtr("Email", p.Email, maxEmailLen),
		domain.CheckLenPtr("Bio", p.Bio, domain.MaxBioLen),
		domain.CheckLenPtr("Kampus", p.Campus, domain.MaxShortLen),
		checkStore(p.Store),
	)
}

// checkStore bounds the shop profile.
func checkStore(st *domain.StoreInfo) error {
	if st == nil {
		return nil
	}
	return domain.FirstErr(
		domain.CheckLen("Nama toko", strings.TrimSpace(st.Name), domain.MaxNameLen),
		domain.CheckLen("Jam buka toko", st.Hours, domain.MaxShortLen),
		domain.CheckLenPtr("NIB", st.NIB, 32),
	)
}
