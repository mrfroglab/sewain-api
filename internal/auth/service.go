// Package auth implements phone OTP login (F2.1), sessions (F2.2), consents (F2.3) and authorization helpers
// (F2.5). It is a self-contained package: wire it into the server with Service.Middleware and
// Handlers.Register (see docs/integration/auth.md).
package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mrfroglab/sewain-api/internal/crypto"
)

// UserDirectory is implemented by the core user repository.
type UserDirectory interface {
	FindUserByPhone(ctx context.Context, phoneE164 string) (userID string, found bool, err error)
	CreateUser(ctx context.Context, tx pgx.Tx, phoneE164 string) (userID string, err error)
	GetUserAuth(ctx context.Context, userID string) (role string, status string, err error) // role user|admin, status aktif|suspend|blacklist
}

// Viewer is the authenticated principal of a request. A guest has an empty UserID.
type Viewer struct{ UserID, Role, SessionID string }

// IsGuest reports whether no user is signed in.
func (v Viewer) IsGuest() bool { return v.UserID == "" }

// IsAdmin reports whether the viewer has the admin role.
func (v Viewer) IsAdmin() bool { return v.UserID != "" && v.Role == RoleAdmin }

// Roles and statuses as stored in users.
const (
	RoleUser        = "user"
	RoleAdmin       = "admin"
	StatusActive    = "aktif"
	StatusSuspended = "suspend"
	StatusBlacklist = "blacklist"
)

// Service holds the auth state machine. All times come from Now so tests can use a fake clock.
type Service struct {
	DB     DB
	Users  UserDirectory
	Sender Sender
	Config Config
	Now    func() time.Time
}

// NewService validates the configuration and returns a Service.
func NewService(db DB, users UserDirectory, sender Sender, cfg Config) (*Service, error) {
	if db == nil || users == nil || sender == nil {
		return nil, errors.New("auth: db, users and sender are required")
	}
	if err := ValidateConfig(cfg.AppEnv, cfg.DemoMode, sender.Name()); err != nil {
		return nil, err
	}
	if len(cfg.OTPPepper) < 32 {
		return nil, errors.New("auth: OTP pepper must be at least 32 bytes")
	}
	if cfg.TermsVersion == "" || cfg.PrivacyVersion == "" {
		return nil, errors.New("auth: terms and privacy versions are required")
	}
	return &Service{DB: db, Users: users, Sender: sender, Config: cfg, Now: time.Now}, nil
}

func (s *Service) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

// RequestMeta is client metadata recorded with challenges, sessions and consents.
type RequestMeta struct {
	IP        string
	UserAgent string
}

func newID(prefix string) (string, error) {
	b, err := crypto.RandomBytes(12)
	if err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func newToken() (string, []byte, error) {
	b, err := crypto.RandomBytes(32)
	if err != nil {
		return "", nil, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	return tok, hashToken(tok), nil
}

func hashToken(tok string) []byte { return crypto.SHA256([]byte(tok)) }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
