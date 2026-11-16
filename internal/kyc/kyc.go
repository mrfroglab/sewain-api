// Package kyc implements identity verification (F2.4, F2.6): encrypted KTP + selfie submissions, one NIK per
// account (FR-A3), the NIK blacklist, the admin review queue with audited file access, and file purging
// 30 days after a decision. Plain NIKs, names and file bytes never reach the database or the blob store.
package kyc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mrfroglab/sewain-api/internal/auth"
	"github.com/mrfroglab/sewain-api/internal/crypto"
)

// BlobStore stores encrypted files. Same shape as media.Storage.
type BlobStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
}

// Auditor writes the append-only audit log (core audit_log).
type Auditor interface {
	Audit(ctx context.Context, actorID, action, target string, oldValue, newValue *string) error
}

// Hooks let the core service mirror KYC state into its own tables (verification_requests, users) inside the
// same transaction. Both are optional; an error aborts the operation.
type Hooks struct {
	// OnSubmitted runs after the submission row is inserted (status pending, or already decided when the
	// provider decided automatically).
	OnSubmitted func(ctx context.Context, tx pgx.Tx, s Submission) error
	// OnDecided runs after a submission is approved or rejected (by an admin or automatically).
	OnDecided func(ctx context.Context, tx pgx.Tx, s Submission) error
}

// Statuses, decisions and file kinds.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"

	FileKTP    = "ktp"
	FileSelfie = "selfie"

	// FileRetention is how long files are kept after a decision (D7).
	FileRetention = 30 * 24 * time.Hour
	// MaxFileBytes is the per-file upload limit.
	MaxFileBytes = 8 << 20
)

// Config holds the KYC secrets.
type Config struct {
	Keys      *crypto.KeyRing // KYC_KEYS
	NIKPepper []byte          // NIK_PEPPER; must never change (it keys the uniqueness/blacklist lookups)
}

// ConfigFromEnv reads KYC_KEYS and NIK_PEPPER. Both are required outside development/demo; in development
// and demo, missing values get ephemeral random secrets (data becomes unreadable after a restart).
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	appEnv := strings.TrimSpace(getenv("APP_ENV"))
	if appEnv == "" {
		appEnv = "development"
	}
	ephemeralOK := appEnv == "development" || appEnv == "demo"
	var c Config
	if spec := strings.TrimSpace(getenv("KYC_KEYS")); spec != "" {
		kr, err := crypto.ParseKeyRing(spec)
		if err != nil {
			return c, fmt.Errorf("kyc: KYC_KEYS: %w", err)
		}
		c.Keys = kr
	} else if ephemeralOK {
		k, err := crypto.RandomBytes(32)
		if err != nil {
			return c, err
		}
		c.Keys = &crypto.KeyRing{}
		if err := c.Keys.Add("ephemeral", k); err != nil {
			return c, err
		}
	} else {
		return c, errors.New("kyc: KYC_KEYS is required outside development/demo")
	}
	if p := strings.TrimSpace(getenv("NIK_PEPPER")); p != "" {
		b, err := crypto.DecodeSecret(p)
		if err != nil {
			return c, fmt.Errorf("kyc: NIK_PEPPER: %w", err)
		}
		c.NIKPepper = b
	} else if ephemeralOK {
		b, err := crypto.RandomBytes(32)
		if err != nil {
			return c, err
		}
		c.NIKPepper = b
	} else {
		return c, errors.New("kyc: NIK_PEPPER is required outside development/demo")
	}
	return c, nil
}

// Service is the KYC state machine.
type Service struct {
	DB       auth.DB
	Blobs    BlobStore
	Auditor  Auditor
	Provider KYCProvider
	Config   Config
	Hooks    Hooks
	Now      func() time.Time
}

// NewService returns a Service using the manual provider unless another is set later.
func NewService(db auth.DB, blobs BlobStore, auditor Auditor, cfg Config) (*Service, error) {
	if db == nil || blobs == nil || auditor == nil {
		return nil, errors.New("kyc: db, blobs and auditor are required")
	}
	if cfg.Keys == nil || len(cfg.NIKPepper) < 32 {
		return nil, errors.New("kyc: key ring and a 32-byte NIK pepper are required")
	}
	return &Service{DB: db, Blobs: blobs, Auditor: auditor, Provider: ManualProvider{}, Config: cfg, Now: time.Now}, nil
}

func (s *Service) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

// Submission is a KYC submission. NameOnKTP is only filled for admin views.
type Submission struct {
	ID              string     `json:"id"`
	UserID          string     `json:"userId"`
	NIKMasked       string     `json:"nikMasked"`
	NameOnKTP       string     `json:"nameOnKtp,omitempty"`
	Status          string     `json:"status"`
	Reason          string     `json:"reason"`
	Provider        string     `json:"provider"`
	CreatedAt       time.Time  `json:"createdAt"`
	DecidedAt       *time.Time `json:"decidedAt"`
	DecidedBy       string     `json:"decidedBy,omitempty"`
	FilesPurgeAfter *time.Time `json:"filesPurgeAfter"`
	FilesPurgedAt   *time.Time `json:"filesPurgedAt"`
}

func (s *Service) nikHMAC(nik string) []byte {
	return crypto.HMAC(s.Config.NIKPepper, []byte("nik"), []byte(nik))
}

func aad(kind, id string) []byte { return []byte("kyc:" + kind + ":" + id) }

func objectKey(id, kind string) string { return "kyc/" + id + "/" + kind }
