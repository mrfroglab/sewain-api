-- OTP daily send cap (OTP_MAX_SENDS_PER_DAY, anti SMS/WA pumping). Owned by the auth package.
-- +goose Up
-- The cap counts every challenge created since 00:00 WIB.
CREATE INDEX otp_challenges_created_idx ON otp_challenges (created_at);

-- One row per WIB day on which the cap was hit: the admins are alerted once per day, on any instance.
CREATE TABLE otp_daily_cap_alerts (
    day         date PRIMARY KEY,                     -- WIB calendar day
    cap         integer NOT NULL CHECK (cap > 0),
    reached_at  timestamptz NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS otp_daily_cap_alerts;
DROP INDEX IF EXISTS otp_challenges_created_idx;
