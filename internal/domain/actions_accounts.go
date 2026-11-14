package domain

import (
	"regexp"
	"strings"
)

var nonDigits = regexp.MustCompile(`\D`)

type RegisterInput struct {
	Name        string     `json:"name"`
	Phone       string     `json:"phone"`
	Email       *string    `json:"email,omitempty"`
	AccountType string     `json:"accountType"`
	Store       *StoreInfo `json:"store,omitempty"`
	AreaID      string     `json:"areaId"`
}

func (e *Engine) RegisterUser(in RegisterInput, wantID string) (ID, error) {
	phone := nonDigits.ReplaceAllString(in.Phone, "")
	if len(phone) < 9 {
		return "", fail("Nomor HP tidak valid.")
	}
	if strings.TrimSpace(in.Name) == "" {
		return "", fail("Nama wajib diisi.")
	}
	for _, u := range e.d.Users {
		if u.Phone == phone {
			return "", fail("Nomor HP sudah terdaftar. Masuk pakai nomor itu.")
		}
	}
	if in.AccountType != "toko" {
		in.AccountType = "individu"
	}
	if _, ok := Areas[in.AreaID]; !ok {
		in.AreaID = "ugm"
	}
	id := pickID(wantID, "u", func(x ID) bool { return e.user(x) != nil })
	u := &User{ID: id, Name: strings.TrimSpace(in.Name), Phone: phone, Email: in.Email, Role: "user", AccountType: in.AccountType, Store: in.Store,
		Verification: Verification{Status: "belum"}, AreaID: in.AreaID, JoinedAt: e.Now(), Status: "aktif"}
	e.d.Users = append(e.d.Users, u)
	e.touch("users", id)
	e.notify(id, "verifikasi", "Selamat datang di Sewain!", "Verifikasi KTP + selfie dulu supaya bisa menyewa dan menyewakan.", "/verifikasi")
	return id, nil
}

type UserPatch struct {
	Name        *string    `json:"name,omitempty"`
	Nickname    *string    `json:"nickname,omitempty"`
	Email       *string    `json:"email,omitempty"`
	Bio         *string    `json:"bio,omitempty"`
	AreaID      *string    `json:"areaId,omitempty"`
	AccountType *string    `json:"accountType,omitempty"`
	Store       *StoreInfo `json:"store,omitempty"`
	Campus      *string    `json:"campus,omitempty"`
}

func (e *Engine) UpdateUser(userID ID, p UserPatch) error {
	u := e.user(userID)
	if u == nil {
		return fail("Akun tidak ditemukan.")
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
		if _, ok := Areas[*p.AreaID]; ok {
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
	e.touch("users", userID)
	return nil
}

type VerificationInput struct {
	Method        string `json:"method"`
	NameOnKtp     string `json:"nameOnKtp"`
	Nik           string `json:"nik"`
	KtpPhotoID    *ID    `json:"ktpPhotoId,omitempty"`
	SelfiePhotoID *ID    `json:"selfiePhotoId,omitempty"`
}

// SubmitVerification: prototype eKYC is simulated and approves instantly (PRD §7.2), but the rules are real:
// one NIK = one account (FR-A3) and blacklisted NIKs cannot come back.
func (e *Engine) SubmitVerification(userID ID, in VerificationInput, wantID string) (ID, error) {
	if e.user(userID) == nil {
		return "", fail("Akun tidak ditemukan.")
	}
	nik := nonDigits.ReplaceAllString(in.Nik, "")
	masked := MaskNik(nik)
	for _, x := range e.d.Users {
		if x.ID != userID && len(nik) >= 8 && deref(x.Verification.NikMasked, "") == masked {
			if x.Status == "blacklist" {
				return "", fail("NIK ini diblokir dari Sewain.")
			}
			return "", fail("NIK ini sudah dipakai akun lain. Satu NIK hanya untuk satu akun.")
		}
	}
	method := in.Method
	if method != "ekyc" && method != "demo" && method != "manual" {
		method = "demo"
	}
	t := e.Now()
	id := pickID(wantID, "v", func(x ID) bool {
		for _, v := range e.d.Verifications {
			if v.ID == x {
				return true
			}
		}
		return false
	})
	v := &VerificationRequest{ID: id, UserID: userID, SubmittedAt: t, Method: method, NameOnKtp: strings.ToUpper(in.NameOnKtp), NikMasked: masked,
		KtpPhotoID: in.KtpPhotoID, SelfiePhotoID: in.SelfiePhotoID, Status: "disetujui", ReviewedAt: ptr(t), Auto: true}
	e.d.Verifications = append([]*VerificationRequest{v}, e.d.Verifications...)
	e.touch("verifications", id)
	u := e.user(userID)
	u.Verification = Verification{Status: "terverifikasi", Method: ptr(method), VerifiedAt: ptr(t), NikMasked: ptr(masked)}
	e.touch("users", userID)
	e.notify(userID, "verifikasi", "Identitas terverifikasi", "Sekarang kamu bisa menyewa dan menyewakan barang.", "/profil")
	return id, nil
}

func (e *Engine) ReviewVerification(adminID, requestID ID, approve bool, reason *string) error {
	if !e.isAdmin(adminID) {
		return fail("Hanya admin.")
	}
	var req *VerificationRequest
	for _, v := range e.d.Verifications {
		if v.ID == requestID {
			req = v
		}
	}
	if req == nil {
		return fail("Pengajuan tidak ditemukan.")
	}
	t := e.Now()
	req.Status, req.ReviewedAt, req.ReviewedBy, req.Reason = "ditolak", ptr(t), ptr(adminID), reason
	if approve {
		req.Status = "disetujui"
	}
	e.touch("verifications", req.ID)
	if u := e.user(req.UserID); u != nil {
		u.Verification.Method = ptr("manual")
		u.Verification.NikMasked = ptr(req.NikMasked)
		if approve {
			u.Verification.Status, u.Verification.VerifiedAt = "terverifikasi", ptr(t)
			e.notify(u.ID, "verifikasi", "Identitas terverifikasi", "Akunmu sudah terverifikasi oleh admin.", "/profil")
		} else {
			u.Verification.Status, u.Verification.VerifiedAt = "ditolak", nil
			e.notify(u.ID, "verifikasi", "Verifikasi ditolak", "Alasan: "+deref(reason, "-")+". Silakan ulangi verifikasi.", "/verifikasi")
		}
		e.touch("users", u.ID)
		action := "verifikasi.tolak"
		if approve {
			action = "verifikasi.setuju"
		}
		e.audit(adminID, action, PublicName(u), nil, reason)
	}
	return nil
}
