package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session is the public view of an auth_sessions row (no token, no hash).
type Session struct {
	ID         string    `json:"id"`
	UserID     string    `json:"-"`
	CreatedAt  time.Time `json:"createdAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	UserAgent  string    `json:"userAgent"`
	IP         string    `json:"ip"`
	Current    bool      `json:"current"`
}

func (s *Service) createSession(ctx context.Context, tx pgx.Tx, userID string, now time.Time, meta RequestMeta) (string, Session, error) {
	tok, hash, err := newToken()
	if err != nil {
		return "", Session{}, err
	}
	id, err := newID("s-")
	if err != nil {
		return "", Session{}, err
	}
	sess := Session{ID: id, UserID: userID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(SessionTTL),
		UserAgent: truncate(meta.UserAgent, 512), IP: truncate(meta.IP, 64)}
	_, err = tx.Exec(ctx, `
		INSERT INTO auth_sessions (id, token_hash, user_id, created_at, last_seen_at, expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7)`, id, hash, userID, now, sess.ExpiresAt, sess.UserAgent, sess.IP)
	return tok, sess, err
}

// CreateSession signs a user in without OTP (demo login, tests). It returns the raw token.
func (s *Service) CreateSession(ctx context.Context, userID string, meta RequestMeta) (string, Session, error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return "", Session{}, err
	}
	defer tx.Rollback(ctx)
	tok, sess, err := s.createSession(ctx, tx, userID, s.now(), meta)
	if err != nil {
		return "", Session{}, err
	}
	return tok, sess, tx.Commit(ctx)
}

// ErrNoSession means the token is unknown, expired, revoked, or belongs to a blocked account.
var ErrNoSession = errors.New("auth: no valid session")

// Resolve maps a raw token to a Viewer. last_seen_at (and the sliding expiry) is written at most every
// SessionTouchEvery; touched reports whether that happened (the middleware then refreshes the cookie).
func (s *Service) Resolve(ctx context.Context, token string) (v Viewer, touched bool, err error) {
	if token == "" || len(token) > 128 {
		return Viewer{}, false, ErrNoSession
	}
	now := s.now()
	var id, userID string
	var lastSeen time.Time
	err = s.DB.QueryRow(ctx, `
		SELECT id, user_id, last_seen_at FROM auth_sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2`, hashToken(token), now).Scan(&id, &userID, &lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return Viewer{}, false, ErrNoSession
	}
	if err != nil {
		return Viewer{}, false, err
	}
	role, status, err := s.Users.GetUserAuth(ctx, userID)
	if err != nil {
		return Viewer{}, false, err
	}
	if status == StatusBlacklist {
		return Viewer{}, false, ErrNoSession
	}
	if now.Sub(lastSeen) >= SessionTouchEvery {
		tag, err := s.DB.Exec(ctx, `
			UPDATE auth_sessions SET last_seen_at = $2, expires_at = $3
			WHERE id = $1 AND revoked_at IS NULL AND last_seen_at = $4`, id, now, now.Add(SessionTTL), lastSeen)
		if err != nil {
			return Viewer{}, false, err
		}
		touched = tag.RowsAffected() == 1
	}
	if role != RoleAdmin {
		role = RoleUser
	}
	return Viewer{UserID: userID, Role: role, SessionID: id}, touched, nil
}

// Sessions lists the user's active sessions, newest activity first. currentID is flagged as current.
func (s *Service) Sessions(ctx context.Context, userID, currentID string) ([]Session, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, user_id, created_at, last_seen_at, expires_at, user_agent, ip FROM auth_sessions
		WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2 ORDER BY last_seen_at DESC, created_at DESC`,
		userID, s.now())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Session, error) {
		var x Session
		err := r.Scan(&x.ID, &x.UserID, &x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt, &x.UserAgent, &x.IP)
		x.Current = x.ID == currentID
		return x, err
	})
}

var errSessionNotFound = userErr(http.StatusNotFound, "Sesi tidak ditemukan.")

// RevokeSession revokes one of the user's sessions.
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID string) error {
	tag, err := s.DB.Exec(ctx, `
		UPDATE auth_sessions SET revoked_at = $3 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		sessionID, userID, s.now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errSessionNotFound
	}
	return nil
}

// RevokeOtherSessions revokes every session of the user except keepID. It returns how many were revoked.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keepID string) (int64, error) {
	tag, err := s.DB.Exec(ctx, `
		UPDATE auth_sessions SET revoked_at = $3 WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`,
		userID, keepID, s.now())
	return tag.RowsAffected(), err
}

// RevokeAllSessions revokes every session of the user (e.g. after a blacklist or account deletion).
func (s *Service) RevokeAllSessions(ctx context.Context, userID string) (int64, error) {
	return s.RevokeOtherSessions(ctx, userID, "")
}

// PurgeExpiredSessions deletes sessions that expired or were revoked before cutoff. Run it from a periodic job.
func (s *Service) PurgeExpiredSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.DB.Exec(ctx, `DELETE FROM auth_sessions WHERE expires_at < $1 OR revoked_at < $1`, cutoff)
	return tag.RowsAffected(), err
}

// PurgeOTPChallenges deletes challenges created before cutoff (keep at least one hour for rate limits).
func (s *Service) PurgeOTPChallenges(ctx context.Context, cutoff time.Time) (int64, error) {
	if s.now().Sub(cutoff) < time.Hour {
		return 0, errors.New("auth: OTP purge cutoff must be at least one hour in the past")
	}
	tag, err := s.DB.Exec(ctx, `DELETE FROM otp_challenges WHERE created_at < $1`, cutoff)
	return tag.RowsAffected(), err
}
