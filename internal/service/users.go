package service

import (
	"context"
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
	if err := domain.CheckLen("Nama", strings.TrimSpace(name), domain.MaxNameLen); err != nil {
		return nil, Change{}, err
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

// Publish tells the change listeners about a change committed outside Service.run (see CreateUser). Prefer
// PublishTx before the commit when the caller owns the transaction.
func (s *Service) Publish(ch Change) {
	if ch.Version > 0 {
		s.emit(ch)
	}
}

// PublishTx runs the transaction-change hooks (OnTxChange) for a change recorded in a transaction the service does
// not own (CreateUser, KYC hooks). The realtime publisher queues a NOTIFY there, delivered only if it commits.
func (s *Service) PublishTx(ctx context.Context, tx pgx.Tx, ch Change) error {
	if ch.Version == 0 {
		return nil
	}
	s.mu.RLock()
	hooks := append([]func(context.Context, pgx.Tx, Change) error{}, s.txListeners...)
	s.mu.RUnlock()
	for _, h := range hooks {
		if err := h(ctx, tx, ch); err != nil {
			return err
		}
	}
	return nil
}

// SetRoleByPhone gives or takes the admin role (CLI `api admin grant|revoke <phone>`, F2.5). The user must exist
// (signed in once). Audited with the given actor.
func (s *Service) SetRoleByPhone(ctx context.Context, phone, role, actor string) (string, error) {
	if role != "admin" && role != "user" {
		return "", domain.Fail("Role harus admin atau user.")
	}
	var id string
	_, err := s.run(ctx, func(t *txn) error {
		r, err := t.q.FindUserByPhone(t.ctx, NormalizePhone(phone))
		if !t.found(err) {
			if t.err == nil {
				return domain.Fail("Tidak ada akun dengan nomor itu. Pengguna harus masuk sekali dulu.")
			}
			return nil
		}
		id = r.ID
		_, err = t.tx.Exec(t.ctx, `UPDATE users SET role = $2, updated_at = now() WHERE id = $1`, r.ID, role)
		t.check(err)
		t.touch(r.ID)
		t.touchPublic()
		t.audit(actor, "user.role", r.ID, domain.Ptr(r.Role), domain.Ptr(role))
		return nil
	})
	return id, err
}
