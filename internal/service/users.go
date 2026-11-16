package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mrfroglab/sewain-api/internal/db/dbq"
	"github.com/mrfroglab/sewain-api/internal/domain"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// User returns a user's full profile (nil when unknown).
func (s *Service) User(ctx context.Context, id string) (*domain.User, error) {
	r, err := s.q.GetUser(ctx, id)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	var store *dbq.Store
	if st, err := s.q.GetStore(ctx, id); err == nil {
		store = &st
	} else if !isNoRows(err) {
		return nil, err
	}
	return userFromRow(r, store), nil
}

// UserAuth is what authorisation needs about a user.
type UserAuth struct {
	ID                 string
	Role               string // user | admin
	Status             string // aktif | suspend | blacklist
	VerificationStatus string
}

// GetUserAuth returns role and status (nil when unknown). For the auth module.
func (s *Service) GetUserAuth(ctx context.Context, id string) (*UserAuth, error) {
	r, err := s.q.GetUserAuth(ctx, id)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &UserAuth{ID: r.ID, Role: r.Role, Status: r.Status, VerificationStatus: r.VerificationStatus}, nil
}

// NormalizePhone stores phone numbers like v1 (digits, local 0-prefix): "+62 812-3456" → "0812 3456" digits.
func NormalizePhone(phone string) string {
	d := domain.Digits(phone)
	if strings.HasPrefix(d, "62") {
		d = "0" + d[2:]
	}
	return d
}

// FindUserByPhone returns the user with that phone (nil when none). For the auth module (OTP login).
func (s *Service) FindUserByPhone(ctx context.Context, phone string) (*domain.User, error) {
	r, err := s.q.FindUserByPhone(ctx, NormalizePhone(phone))
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return s.User(ctx, r.ID)
}

// CreateUser creates a new account inside the caller's transaction (the auth module creates the account when an
// OTP is verified, together with its own session/consent rows). The welcome notification is sent like
// registerUser. Duplicate phone or e-mail returns a domain.UserError. Returns the change to publish after commit
// (pass it to Service.Publish).
func (s *Service) CreateUser(ctx context.Context, tx pgx.Tx, phone, name string) (*domain.User, Change, error) {
	t := newTxn(ctx, s, tx)
	if err := t.loadClock(); err != nil {
		return nil, Change{}, err
	}
	phone = NormalizePhone(phone)
	if len(phone) < 9 {
		return nil, Change{}, domain.Fail("Nomor HP tidak valid.")
	}
	if strings.TrimSpace(name) == "" {
		return nil, Change{}, domain.Fail("Nama wajib diisi.")
	}
	u := &domain.User{ID: domain.NewID("u"), Name: strings.TrimSpace(name), Phone: phone, Role: "user", AccountType: "individu",
		Verification: domain.Verification{Status: "belum"}, AreaID: "ugm", JoinedAt: t.now, Status: "aktif"}
	if err := t.insertUser(u); err != nil {
		return nil, Change{}, err
	}
	t.notify(u.ID, "verifikasi", "Selamat datang di Sewain!", "Verifikasi KTP + selfie dulu supaya bisa menyewa dan menyewakan.", "/verifikasi")
	if t.err != nil {
		return nil, Change{}, t.err
	}
	ch, err := t.commitChange()
	return u, ch, err
}

// Publish tells the change listeners about a change committed outside Service.run (see CreateUser).
func (s *Service) Publish(ch Change) {
	if ch.Version > 0 {
		s.emit(ch)
	}
}

/* ---------------------------------------------------------- demo sessions */

// HashToken is how session tokens are stored (never the token itself).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateDemoSession issues a bearer token for a user (DEMO_MODE persona login). Only its SHA-256 is stored.
func (s *Service) CreateDemoSession(ctx context.Context, userID string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	return token, s.q.InsertDemoSession(ctx, dbq.InsertDemoSessionParams{TokenHash: HashToken(token), UserID: userID})
}

// DemoSessionUser resolves a demo token to its user id ("" when unknown).
func (s *Service) DemoSessionUser(ctx context.Context, token string) (string, error) {
	uid, err := s.q.DemoSessionUser(ctx, HashToken(token))
	if isNoRows(err) {
		return "", nil
	}
	return uid, err
}
