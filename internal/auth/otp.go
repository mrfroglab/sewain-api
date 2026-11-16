package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mrfroglab/sewain-api/internal/crypto"
	"github.com/mrfroglab/sewain-api/internal/platform/ratelimit"
)

// PurposeLogin is the only OTP purpose: it signs in and registers in one step.
const PurposeLogin = "login"

// RequestResult is returned by RequestOTP. It never reveals whether the phone is registered.
type RequestResult struct {
	Phone       string        // normalized E.164
	ExpiresAt   time.Time     //
	ResendAfter time.Duration // wait before asking again
	DevCode     string        // only for the echo/demo providers
}

// RequestOTP issues a login code for phone. Rate limits are counted from otp_challenges inside a transaction
// serialized by advisory locks on the phone and IP, so they hold across instances.
func (s *Service) RequestOTP(ctx context.Context, rawPhone string, meta RequestMeta) (RequestResult, error) {
	phone, err := NormalizePhone(rawPhone)
	if err != nil {
		return RequestResult{}, err
	}
	now := s.now()
	code, err := s.newCode()
	if err != nil {
		return RequestResult{}, err
	}
	id, err := newID("otp-")
	if err != nil {
		return RequestResult{}, err
	}
	// The per-IP limit counts IPv6 clients per /64 (one subscriber owns the whole prefix), so the column
	// holds the /64 for IPv6 and the address for IPv4.
	ip := truncate(ratelimit.IPBucket(strings.TrimSpace(meta.IP)), 64)

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return RequestResult{}, err
	}
	defer tx.Rollback(ctx)
	// Lock order is always phone, then IP, so concurrent requests cannot deadlock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('otp:phone:' || $1, 0))`, phone); err != nil {
		return RequestResult{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('otp:ip:' || $1, 0))`, ip); err != nil {
		return RequestResult{}, err
	}
	hourAgo := now.Add(-time.Hour)
	var phoneCount int
	var lastAt *time.Time
	var oldestAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT count(*), max(created_at), min(created_at) FROM otp_challenges
		WHERE phone = $1 AND created_at > $2`, phone, hourAgo).Scan(&phoneCount, &lastAt, &oldestAt); err != nil {
		return RequestResult{}, err
	}
	if lastAt != nil && now.Sub(*lastAt) < OTPResendGap {
		wait := OTPResendGap - now.Sub(*lastAt)
		return RequestResult{}, &Error{Status: http.StatusTooManyRequests, RetryAfter: wait,
			Msg: fmt.Sprintf("Tunggu %d detik sebelum minta kode lagi.", int((wait+time.Second-1)/time.Second))}
	}
	if phoneCount >= OTPMaxPerPhoneHour {
		return RequestResult{}, &Error{Status: http.StatusTooManyRequests, RetryAfter: oldestAt.Add(time.Hour).Sub(now),
			Msg: "Terlalu sering minta kode untuk nomor ini. Coba lagi nanti."}
	}
	if ip != "" {
		var ipCount int
		var ipOldest *time.Time
		if err := tx.QueryRow(ctx, `
			SELECT count(*), min(created_at) FROM otp_challenges WHERE ip = $1 AND created_at > $2`,
			ip, hourAgo).Scan(&ipCount, &ipOldest); err != nil {
			return RequestResult{}, err
		}
		if ipCount >= OTPMaxPerIPHour {
			return RequestResult{}, &Error{Status: http.StatusTooManyRequests, RetryAfter: ipOldest.Add(time.Hour).Sub(now),
				Msg: "Terlalu banyak permintaan kode dari jaringanmu. Coba lagi nanti."}
		}
	}
	expires := now.Add(OTPTTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO otp_challenges (id, phone, purpose, code_hash, ip, provider, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		id, phone, PurposeLogin, s.codeHash(id, phone, code), ip, s.Sender.Name(), now, expires); err != nil {
		return RequestResult{}, err
	}
	// Send before commit: if the gateway fails, the challenge is rolled back and does not burn the quota.
	if err := s.Sender.Send(ctx, OTPMessage{Phone: phone, Code: code}); err != nil {
		return RequestResult{}, &Error{Status: http.StatusBadGateway, Msg: "Kode OTP gagal dikirim. Coba lagi sebentar lagi.", Code: "otp_send_failed"}
	}
	if err := tx.Commit(ctx); err != nil {
		return RequestResult{}, err
	}
	res := RequestResult{Phone: phone, ExpiresAt: expires, ResendAfter: OTPResendGap}
	if _, ok := s.Sender.(FixedCoder); ok {
		res.DevCode = code
	}
	if e, ok := s.Sender.(CodeEchoer); ok && e.EchoCode() {
		res.DevCode = code
	}
	return res, nil
}

func (s *Service) newCode() (string, error) {
	if f, ok := s.Sender.(FixedCoder); ok {
		return f.FixedCode(), nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (s *Service) codeHash(id, phone, code string) []byte {
	return crypto.HMAC(s.Config.OTPPepper, []byte("otp"), []byte(id), []byte(phone), []byte(code))
}

// VerifyInput is the OTP verification request.
type VerifyInput struct {
	Phone       string
	Code        string
	AcceptTerms bool // required when the phone is not registered yet
}

// VerifyResult is returned on successful verification.
type VerifyResult struct {
	UserID  string
	Role    string
	IsNew   bool
	Token   string // raw session token: set it as cookie or hand it to bearer clients, never log it
	Session Session
}

var (
	errCodeInvalid  = userErr(http.StatusBadRequest, "Kode OTP salah atau sudah kedaluwarsa. Minta kode baru kalau perlu.")
	errCodeLocked   = userErr(http.StatusTooManyRequests, "Terlalu banyak percobaan kode salah. Minta kode baru.")
	errConsent      = &Error{Status: http.StatusBadRequest, Code: "consent_required", Msg: "Nomor ini belum terdaftar. Setujui Syarat & Ketentuan dan Kebijakan Privasi untuk membuat akun."}
	errBlacklisted  = userErr(http.StatusForbidden, "Akun ini diblokir. Hubungi admin kalau menurutmu ini keliru.")
	errAccountState = userErr(http.StatusForbidden, "Akun ini tidak bisa masuk saat ini. Hubungi admin.")
)

// VerifyOTP checks the latest challenge for the phone, then signs the user in (creating the account and
// recording consents when the phone is new). Wrong codes count towards OTPMaxAttempts.
func (s *Service) VerifyOTP(ctx context.Context, in VerifyInput, meta RequestMeta) (VerifyResult, error) {
	phone, err := NormalizePhone(in.Phone)
	if err != nil {
		return VerifyResult{}, err
	}
	if len(in.Code) != 6 {
		return VerifyResult{}, errCodeInvalid
	}
	now := s.now()
	userID, found, err := s.Users.FindUserByPhone(ctx, phone)
	if err != nil {
		return VerifyResult{}, err
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return VerifyResult{}, err
	}
	defer tx.Rollback(ctx)

	var id string
	var hash []byte
	var attempts int
	var expiresAt time.Time
	var consumedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT id, code_hash, attempts, expires_at, consumed_at FROM otp_challenges
		WHERE phone = $1 AND purpose = $2 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		phone, PurposeLogin).Scan(&id, &hash, &attempts, &expiresAt, &consumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifyResult{}, errCodeInvalid
	}
	if err != nil {
		return VerifyResult{}, err
	}
	if consumedAt != nil || !now.Before(expiresAt) {
		return VerifyResult{}, errCodeInvalid
	}
	if attempts >= OTPMaxAttempts {
		return VerifyResult{}, errCodeLocked
	}
	if !crypto.Equal(hash, s.codeHash(id, phone, in.Code)) {
		if _, err := tx.Exec(ctx, `UPDATE otp_challenges SET attempts = attempts + 1 WHERE id = $1`, id); err != nil {
			return VerifyResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return VerifyResult{}, err
		}
		if attempts+1 >= OTPMaxAttempts {
			return VerifyResult{}, errCodeLocked
		}
		return VerifyResult{}, errCodeInvalid
	}
	// Correct code. A new account needs consent first; keep the challenge usable so the client can retry
	// with acceptTerms without requesting another code.
	if !found && !in.AcceptTerms {
		return VerifyResult{}, errConsent
	}
	if _, err := tx.Exec(ctx, `UPDATE otp_challenges SET consumed_at = $2 WHERE id = $1`, id, now); err != nil {
		return VerifyResult{}, err
	}
	role := RoleUser
	if found {
		r, status, err := s.Users.GetUserAuth(ctx, userID)
		if err != nil {
			return VerifyResult{}, err
		}
		switch status {
		case StatusBlacklist:
			// Commit so the code cannot be replayed.
			if err := tx.Commit(ctx); err != nil {
				return VerifyResult{}, err
			}
			return VerifyResult{}, errBlacklisted
		case StatusActive, StatusSuspended: // suspended users may sign in; core restricts their actions
		default:
			if err := tx.Commit(ctx); err != nil {
				return VerifyResult{}, err
			}
			return VerifyResult{}, errAccountState
		}
		role = r
	} else {
		userID, err = s.Users.CreateUser(ctx, tx, phone)
		if err != nil {
			return VerifyResult{}, err
		}
		if err := s.recordConsents(ctx, tx, userID, now, meta); err != nil {
			return VerifyResult{}, err
		}
	}
	tok, sess, err := s.createSession(ctx, tx, userID, now, meta)
	if err != nil {
		return VerifyResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{UserID: userID, Role: role, IsNew: !found, Token: tok, Session: sess}, nil
}
