-- name: GetUser :one
SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserForUpdate :one
SELECT * FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE;

-- name: ListUsers :many
SELECT * FROM users WHERE deleted_at IS NULL ORDER BY ord;

-- name: UserExists :one
SELECT EXISTS (SELECT 1 FROM users WHERE id = $1);

-- name: PhoneTaken :one
SELECT EXISTS (SELECT 1 FROM users WHERE phone = $1 AND deleted_at IS NULL);

-- name: FindUserByPhone :one
SELECT * FROM users WHERE phone = $1 AND deleted_at IS NULL;

-- name: GetUserAuth :one
SELECT id, role, status, verification_status FROM users WHERE id = $1 AND deleted_at IS NULL;

-- name: OtherUserByNikMasked :one
SELECT id, status FROM users
WHERE nik_masked = @nik_masked AND id <> @user_id AND deleted_at IS NULL
ORDER BY ord LIMIT 1;

-- name: InsertUser :exec
INSERT INTO users (id, phone, name, nickname, email, role, account_type, area_id, campus, bio, status, strikes,
  verification_status, verification_method, verified_at, nik_masked, early_lender, student_verified, base_rating,
  base_rating_count, demo, joined_at)
VALUES (@id, @phone, @name, @nickname, @email, @role, @account_type, @area_id, @campus, @bio, @status, @strikes,
  @verification_status, @verification_method, @verified_at, @nik_masked, @early_lender, @student_verified, @base_rating,
  @base_rating_count, @demo, @joined_at);

-- name: UpdateUser :exec
UPDATE users SET name = @name, nickname = @nickname, email = @email, account_type = @account_type, area_id = @area_id,
  campus = @campus, bio = @bio, status = @status, strikes = @strikes, verification_status = @verification_status,
  verification_method = @verification_method, verified_at = @verified_at, nik_masked = @nik_masked, updated_at = now()
WHERE id = @id;

-- name: GetStore :one
SELECT * FROM stores WHERE user_id = $1;

-- name: ListStores :many
SELECT * FROM stores;

-- name: UpsertStore :exec
INSERT INTO stores (user_id, name, area_id, hours, nib) VALUES (@user_id, @name, @area_id, @hours, @nib)
ON CONFLICT (user_id) DO UPDATE SET name = EXCLUDED.name, area_id = EXCLUDED.area_id, hours = EXCLUDED.hours,
  nib = EXCLUDED.nib, updated_at = now();

-- name: DeleteStore :exec
DELETE FROM stores WHERE user_id = $1;

-- name: InsertDemoSession :exec
INSERT INTO demo_sessions (token_hash, user_id) VALUES ($1, $2);

-- name: DemoSessionUser :one
SELECT user_id FROM demo_sessions WHERE token_hash = $1;

/* ------------------------------------------------------------ verification */

-- name: GetVerification :one
SELECT * FROM verification_requests WHERE id = $1;

-- name: GetVerificationForUpdate :one
SELECT * FROM verification_requests WHERE id = $1 FOR UPDATE;

-- name: VerificationExists :one
SELECT EXISTS (SELECT 1 FROM verification_requests WHERE id = $1);

-- name: ListVerifications :many
SELECT * FROM verification_requests ORDER BY ord DESC;

-- name: ListVerificationsByUser :many
SELECT * FROM verification_requests WHERE user_id = $1 ORDER BY ord DESC;

-- name: InsertVerification :exec
INSERT INTO verification_requests (id, ord, user_id, method, name_on_ktp, nik_masked, ktp_photo_id, selfie_photo_id, status,
  auto, submitted_at, reviewed_at, reviewed_by, reason)
VALUES (@id, nextval('row_ord_seq'), @user_id, @method, @name_on_ktp, @nik_masked, @ktp_photo_id, @selfie_photo_id, @status,
  @auto, @submitted_at, @reviewed_at, @reviewed_by, @reason);

-- name: UpdateVerificationReview :exec
UPDATE verification_requests SET status = @status, reviewed_at = @reviewed_at, reviewed_by = @reviewed_by, reason = @reason,
  updated_at = now()
WHERE id = @id;
