package auth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Consent documents (F2.3).
const (
	DocTerms   = "terms"
	DocPrivacy = "privacy"
)

// Consent is one accepted document version.
type Consent struct {
	Doc        string    `json:"doc"`
	Version    string    `json:"version"`
	AcceptedAt time.Time `json:"acceptedAt"`
}

func (s *Service) recordConsents(ctx context.Context, tx pgx.Tx, userID string, now time.Time, meta RequestMeta) error {
	for _, d := range []struct{ doc, ver string }{{DocTerms, s.Config.TermsVersion}, {DocPrivacy, s.Config.PrivacyVersion}} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO consents (user_id, doc, version, accepted_at, ip, user_agent)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (user_id, doc, version) DO NOTHING`,
			userID, d.doc, d.ver, now, truncate(meta.IP, 64), truncate(meta.UserAgent, 512)); err != nil {
			return err
		}
	}
	return nil
}

// AcceptCurrentDocs records consent to the configured terms/privacy versions for an existing user (e.g. when
// the documents change and the web asks the user to accept again). Idempotent.
func (s *Service) AcceptCurrentDocs(ctx context.Context, userID string, meta RequestMeta) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.recordConsents(ctx, tx, userID, s.now(), meta); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Consents lists the documents a user has accepted, newest first.
func (s *Service) Consents(ctx context.Context, userID string) ([]Consent, error) {
	rows, err := s.DB.Query(ctx, `SELECT doc, version, accepted_at FROM consents WHERE user_id = $1 ORDER BY accepted_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Consent, error) {
		var c Consent
		err := r.Scan(&c.Doc, &c.Version, &c.AcceptedAt)
		return c, err
	})
}

// NeedsConsent reports whether the user has not yet accepted the current terms or privacy version.
func (s *Service) NeedsConsent(ctx context.Context, userID string) (bool, error) {
	var n int
	err := s.DB.QueryRow(ctx, `
		SELECT count(*) FROM consents WHERE user_id = $1 AND ((doc = 'terms' AND version = $2) OR (doc = 'privacy' AND version = $3))`,
		userID, s.Config.TermsVersion, s.Config.PrivacyVersion).Scan(&n)
	return n < 2, err
}
