-- name: GetBooking :one
SELECT * FROM bookings WHERE id = $1;

-- name: GetBookingForUpdate :one
SELECT * FROM bookings WHERE id = $1 FOR UPDATE;

-- name: BookingExists :one
SELECT EXISTS (SELECT 1 FROM bookings WHERE id = $1);

-- name: ListBookings :many
SELECT * FROM bookings ORDER BY ord DESC;

-- name: ListBookingsOfUser :many
SELECT * FROM bookings WHERE renter_id = @user_id OR provider_id = @user_id ORDER BY ord DESC;

-- name: NextBookingNumber :one
SELECT nextval('booking_code_seq')::bigint;

-- name: InsertBooking :exec
INSERT INTO bookings (id, code, listing_id, renter_id, provider_id, start_date, end_date, handover, collateral, note, status,
  overdue, created_at, expires_at, accepted_at, declined_at, decline_reason, cancelled_at, cancelled_by, cancel_reason,
  active_at, returned_at, completed_at, completed_auto, resolved_at, agreement, agreement_hash, renter_agreed_at,
  provider_agreed_at, rent_total, collateral_value, commission_amount, commission_charged, commission_bearer, payment,
  meeting_point_detail, checkout, checkin, collateral_return, dispute_id)
VALUES (@id, @code, @listing_id, @renter_id, @provider_id, @start_date, @end_date, @handover, @collateral, @note, @status,
  @overdue, @created_at, @expires_at, @accepted_at, @declined_at, @decline_reason, @cancelled_at, @cancelled_by,
  @cancel_reason, @active_at, @returned_at, @completed_at, @completed_auto, @resolved_at, @agreement, @agreement_hash,
  @renter_agreed_at, @provider_agreed_at, @rent_total, @collateral_value, @commission_amount, @commission_charged,
  @commission_bearer, @payment, @meeting_point_detail, @checkout, @checkin, @collateral_return, @dispute_id);

-- name: UpdateBooking :exec
UPDATE bookings SET status = @status, overdue = @overdue, accepted_at = @accepted_at, declined_at = @declined_at,
  decline_reason = @decline_reason, cancelled_at = @cancelled_at, cancelled_by = @cancelled_by,
  cancel_reason = @cancel_reason, active_at = @active_at, returned_at = @returned_at, completed_at = @completed_at,
  completed_auto = @completed_auto, resolved_at = @resolved_at, renter_agreed_at = @renter_agreed_at,
  provider_agreed_at = @provider_agreed_at, payment = @payment, meeting_point_detail = @meeting_point_detail,
  checkout = @checkout, checkin = @checkin, collateral_return = @collateral_return, dispute_id = @dispute_id,
  updated_at = now()
WHERE id = @id;

-- name: ListBookingsDueForJobs :many
-- Candidates for the periodic job (expiry, overdue, auto-complete), in the snapshot order like v1's tick.
SELECT * FROM bookings
WHERE (status = 'REQUESTED' AND expires_at <= @now)
   OR (status IN ('ACTIVE', 'DISPUTED') AND NOT overdue AND returned_at IS NULL AND end_date < @today)
   OR (status = 'RETURNED' AND collateral_return IS NOT NULL
       AND (collateral_return ->> 'autoCompleteAt') IS NOT NULL
       AND (collateral_return ->> 'renterConfirmedAt') IS NULL
       AND (collateral_return ->> 'autoCompleteAt')::bigint <= @now_ms::bigint)
ORDER BY ord DESC
FOR UPDATE SKIP LOCKED;

/* ------------------------------------------------------------------ photos */

-- name: GetPhoto :one
SELECT * FROM photos WHERE id = $1;

-- name: ListPhotos :many
SELECT * FROM photos ORDER BY ord;

-- name: ListPhotosVisibleTo :many
SELECT * FROM photos p
WHERE p.uploader_id = @viewer
   OR EXISTS (SELECT 1 FROM bookings b WHERE b.id = p.booking_id AND (b.renter_id = @viewer OR b.provider_id = @viewer))
ORDER BY ord;

-- name: CountExistingPhotos :one
SELECT count(*)::int FROM photos WHERE id = ANY (@ids::text[]);

-- name: InsertPhoto :exec
INSERT INTO photos (id, booking_id, listing_id, stage, uploader_id, src, object_key, sha256, server_ts, source, angle, width,
  height, bytes, mime)
VALUES (@id, @booking_id, @listing_id, @stage, @uploader_id, @src, @object_key, @sha256, @server_ts, @source, @angle, @width,
  @height, @bytes, @mime);

/* ---------------------------------------------------------------- messages */

-- name: ListMessages :many
SELECT * FROM messages ORDER BY ord;

-- name: ListMessagesOfUser :many
SELECT m.* FROM messages m JOIN bookings b ON b.id = m.booking_id
WHERE b.renter_id = @user_id OR b.provider_id = @user_id
ORDER BY m.ord;

-- name: InsertMessage :exec
INSERT INTO messages (id, booking_id, sender_id, text, photo_id, at) VALUES (@id, @booking_id, @sender_id, @text, @photo_id, @at);

-- name: ListChatReads :many
SELECT * FROM chat_reads ORDER BY user_id, booking_id;

-- name: ListChatReadsOfUser :many
SELECT * FROM chat_reads WHERE user_id = $1 ORDER BY booking_id;

-- name: UpsertChatRead :exec
INSERT INTO chat_reads (user_id, booking_id, read_at) VALUES (@user_id, @booking_id, @read_at)
ON CONFLICT (user_id, booking_id) DO UPDATE SET read_at = EXCLUDED.read_at;

/* ----------------------------------------------------------------- reviews */

-- name: ListReviews :many
SELECT * FROM reviews ORDER BY ord;

-- name: ListReviewsVisibleTo :many
-- FR-T2: released reviews, plus the viewer's own reviews.
SELECT * FROM reviews WHERE released OR from_id = sqlc.narg(viewer)::text ORDER BY ord;

-- name: ReviewExists :one
SELECT EXISTS (SELECT 1 FROM reviews WHERE booking_id = @booking_id AND from_id = @from_id);

-- name: InsertReview :exec
INSERT INTO reviews (id, booking_id, listing_id, from_id, to_id, from_role, rating, comment, created_at, visible_at, released)
VALUES (@id, @booking_id, @listing_id, @from_id, @to_id, @from_role, @rating, @comment, @created_at, @visible_at, @released);

-- name: ReleaseReviewsOfBooking :exec
-- Both parties reviewed: show both now.
UPDATE reviews SET visible_at = LEAST(visible_at, @now), released = true WHERE booking_id = @booking_id;

-- name: ReleaseDueReviews :many
UPDATE reviews SET released = true WHERE NOT released AND visible_at <= @now RETURNING from_id, to_id;

-- name: ListVisibleReviewsAbout :many
-- Public profile page: reviews about a user that are shown (released, or past visible_at, FR-T2).
SELECT * FROM reviews WHERE to_id = @to_id AND (released OR visible_at <= @now::timestamptz) ORDER BY ord;
