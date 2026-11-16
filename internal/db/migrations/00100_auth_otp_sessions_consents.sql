-- Auth (F2.1–F2.3): OTP challenges, sessions, consents. Owned by the auth package.
-- +goose Up
CREATE TABLE otp_challenges (
    id           text PRIMARY KEY,
    phone        text NOT NULL,                       -- E.164 (+62…)
    purpose      text NOT NULL DEFAULT 'login' CHECK (purpose IN ('login')),
    code_hash    bytea NOT NULL,                      -- HMAC-SHA256(OTP_PEPPER, id|phone|code), never the code
    ip           text NOT NULL DEFAULT '',
    provider     text NOT NULL,
    attempts     integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    created_at   timestamptz NOT NULL,
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,
    CHECK (expires_at > created_at)
);
-- Rate limits (per phone / per IP per hour) and "latest challenge" lookups.
CREATE INDEX otp_challenges_phone_created_idx ON otp_challenges (phone, created_at DESC);
CREATE INDEX otp_challenges_ip_created_idx ON otp_challenges (ip, created_at DESC);

CREATE TABLE auth_sessions (
    id            text PRIMARY KEY,                   -- public handle (s-…), safe to show in session lists
    token_hash    bytea NOT NULL UNIQUE,              -- SHA-256(token); the token itself is never stored
    user_id       text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL,
    expires_at    timestamptz NOT NULL,               -- sliding: last_seen_at + 30 days
    revoked_at    timestamptz,
    user_agent    text NOT NULL DEFAULT '',
    ip            text NOT NULL DEFAULT ''
);
CREATE INDEX auth_sessions_user_idx ON auth_sessions (user_id);
CREATE INDEX auth_sessions_expires_idx ON auth_sessions (expires_at) WHERE revoked_at IS NULL;

CREATE TABLE consents (
    id           bigserial PRIMARY KEY,
    user_id      text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    doc          text NOT NULL CHECK (doc IN ('terms', 'privacy')),
    version      text NOT NULL CHECK (version <> ''),
    accepted_at  timestamptz NOT NULL,
    ip           text NOT NULL DEFAULT '',
    user_agent   text NOT NULL DEFAULT '',
    UNIQUE (user_id, doc, version)
);
CREATE INDEX consents_user_idx ON consents (user_id);

-- +goose Down
DROP TABLE IF EXISTS consents;
DROP TABLE IF EXISTS auth_sessions;
DROP TABLE IF EXISTS otp_challenges;
