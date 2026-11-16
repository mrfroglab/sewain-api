-- +goose Up
-- Demo persona logins now create real auth_sessions rows (hashed token, sliding expiry), so the v1-style demo
-- bearer table is gone. Tokens issued before this migration stop working; the web asks for a new login.
DROP TABLE IF EXISTS demo_sessions;

-- KYC decisions update the core verification request that mirrors a submission.
CREATE INDEX IF NOT EXISTS verification_requests_kyc_idx ON verification_requests (kyc_submission_id)
  WHERE kyc_submission_id IS NOT NULL;

-- Retention: read notifications older than 180 days are deleted.
CREATE INDEX IF NOT EXISTS notifications_read_idx ON notifications (read_at) WHERE read_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS notifications_read_idx;
DROP INDEX IF EXISTS verification_requests_kyc_idx;
CREATE TABLE IF NOT EXISTS demo_sessions (
  token_hash text PRIMARY KEY,
  user_id    text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS demo_sessions_user_idx ON demo_sessions (user_id);
