-- KYC (F2.4): encrypted submissions, verified identities (one NIK = one account, FR-A3), NIK blacklist.
-- Plain NIK and file bytes never reach the database: NIK is stored as HMAC (lookup) + AES-GCM ciphertext,
-- files live encrypted in the blob store and only their object keys are kept here.
-- +goose Up
CREATE TABLE kyc_submissions (
    id                 text PRIMARY KEY,
    user_id            text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    nik_hmac           bytea NOT NULL,
    nik_enc            bytea NOT NULL,
    nik_masked         text NOT NULL,                 -- 3404********0001
    name_enc           bytea NOT NULL,
    ktp_object         text,                          -- blob key; NULL after purge
    selfie_object      text,
    provider           text NOT NULL DEFAULT 'manual',
    status             text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    reason             text NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    decided_at         timestamptz,
    decided_by         text,                          -- admin user id or "system:<provider>"
    files_purge_after  timestamptz,
    files_purged_at    timestamptz,
    CHECK ((status = 'pending') = (decided_at IS NULL))
);
CREATE INDEX kyc_submissions_status_created_idx ON kyc_submissions (status, created_at);
CREATE INDEX kyc_submissions_user_idx ON kyc_submissions (user_id);
CREATE INDEX kyc_submissions_nik_idx ON kyc_submissions (nik_hmac);
CREATE INDEX kyc_submissions_purge_idx ON kyc_submissions (files_purge_after) WHERE files_purged_at IS NULL;
-- At most one open submission per user.
CREATE UNIQUE INDEX kyc_submissions_one_pending_idx ON kyc_submissions (user_id) WHERE status = 'pending';

CREATE TABLE identities (
    user_id      text PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    nik_hmac     bytea NOT NULL UNIQUE,               -- FR-A3: one NIK, one account
    nik_enc      bytea NOT NULL,
    nik_masked   text NOT NULL,
    submission_id text REFERENCES kyc_submissions (id) ON DELETE SET NULL,
    verified_at  timestamptz NOT NULL,
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL
);

CREATE TABLE nik_blacklist (
    nik_hmac    bytea PRIMARY KEY,
    reason      text NOT NULL DEFAULT '',
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS nik_blacklist;
DROP TABLE IF EXISTS identities;
DROP TABLE IF EXISTS kyc_submissions;
