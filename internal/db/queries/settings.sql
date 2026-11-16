-- name: CurrentCommission :one
SELECT * FROM commission_configs ORDER BY version DESC LIMIT 1;

-- name: InsertCommission :exec
INSERT INTO commission_configs (active, mode, percent, flat, min, max, bearer, split_renter_percent, effective_from, created_by)
VALUES (@active, @mode, @percent, @flat, @min, @max, @bearer, @split_renter_percent, @effective_from, @created_by);

-- name: CurrentCollateralRules :one
SELECT * FROM collateral_rule_sets ORDER BY version DESC LIMIT 1;

-- name: InsertCollateralRules :exec
INSERT INTO collateral_rule_sets (low_max, mid_max, mid_deposit_percent, high_deposit_percent, vehicle_deposit_percent,
  high_value_badge, effective_from, created_by)
VALUES (@low_max, @mid_max, @mid_deposit_percent, @high_deposit_percent, @vehicle_deposit_percent, @high_value_badge,
  @effective_from, @created_by);

-- name: ListBannedKeywords :many
SELECT word FROM banned_keywords ORDER BY position;

-- name: DeleteBannedKeywords :exec
DELETE FROM banned_keywords;

-- name: InsertBannedKeywords :exec
INSERT INTO banned_keywords (word, position)
SELECT w.word, (w.ordinality - 1)::int FROM unnest(@words::text[]) WITH ORDINALITY AS w(word, ordinality);

-- name: GetSetting :one
SELECT value FROM app_settings WHERE key = $1;

-- name: PutSetting :exec
INSERT INTO app_settings (key, value) VALUES (@key, @value)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: InsertChange :one
INSERT INTO change_log (version, users, admins, public) VALUES (nextval('state_version_seq'), @users, @admins, @public)
RETURNING version;

-- name: CurrentVersion :one
SELECT coalesce(max(version), 0)::bigint FROM change_log;

-- name: PruneChangeLog :exec
DELETE FROM change_log WHERE version < (SELECT max(version) FROM change_log) - @keep::bigint;

-- name: BookingCounter :one
SELECT coalesce(last_value, 0)::bigint FROM pg_catalog.pg_sequences
WHERE schemaname = current_schema() AND sequencename = 'booking_code_seq';

-- name: SetBookingCounter :exec
SELECT setval('booking_code_seq', greatest(@value::bigint, 1), @value::bigint > 0);

/* ------------------------------------------------------------- idempotency */

-- name: ClaimIdempotencyKey :execrows
INSERT INTO idempotency_keys (user_id, key, action, request_hash, status, response)
VALUES (@user_id, @key, @action, @request_hash, @status, @response)
ON CONFLICT (user_id, key) DO NOTHING;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys WHERE user_id = @user_id AND key = @key;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_keys SET status = @status, response = @response WHERE user_id = @user_id AND key = @key;

-- name: PruneIdempotencyKeys :exec
DELETE FROM idempotency_keys WHERE created_at < @before;
