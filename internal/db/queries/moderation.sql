/* ----------------------------------------------------------------- reports */

-- name: GetReportForUpdate :one
SELECT * FROM reports WHERE id = $1 FOR UPDATE;

-- name: ReportExists :one
SELECT EXISTS (SELECT 1 FROM reports WHERE id = $1);

-- name: ListReports :many
SELECT * FROM reports ORDER BY ord DESC;

-- name: ListReportsByReporter :many
SELECT * FROM reports WHERE reporter_id = $1 ORDER BY ord DESC;

-- name: InsertReport :exec
INSERT INTO reports (id, reporter_id, target_type, target_id, reason, detail, status, resolution, resolved_at, resolved_by, created_at)
VALUES (@id, @reporter_id, @target_type, @target_id, @reason, @detail, @status, @resolution, @resolved_at, @resolved_by, @created_at);

-- name: ResolveReport :exec
UPDATE reports SET status = @status, resolution = @resolution, resolved_at = @resolved_at, resolved_by = @resolved_by WHERE id = @id;

/* ---------------------------------------------------------------- disputes */

-- name: GetDispute :one
SELECT * FROM disputes WHERE id = $1;

-- name: GetDisputeForUpdate :one
SELECT * FROM disputes WHERE id = $1 FOR UPDATE;

-- name: DisputeExists :one
SELECT EXISTS (SELECT 1 FROM disputes WHERE id = $1);

-- name: ListDisputes :many
SELECT * FROM disputes ORDER BY ord DESC;

-- name: ListDisputesOfUser :many
SELECT d.* FROM disputes d JOIN bookings b ON b.id = d.booking_id
WHERE b.renter_id = @user_id OR b.provider_id = @user_id
ORDER BY d.ord DESC;

-- name: ListDisputesDue :many
SELECT * FROM disputes WHERE status = 'negosiasi' AND l1_deadline <= @now ORDER BY ord DESC FOR UPDATE SKIP LOCKED;

-- name: InsertDispute :exec
INSERT INTO disputes (id, booking_id, opened_by, case_type, description, layer, status, l1_deadline, recommendation, outcome,
  resolved_at, created_at)
VALUES (@id, @booking_id, @opened_by, @case_type, @description, @layer, @status, @l1_deadline, @recommendation, @outcome,
  @resolved_at, @created_at);

-- name: UpdateDispute :exec
UPDATE disputes SET layer = @layer, status = @status, recommendation = @recommendation, outcome = @outcome,
  resolved_at = @resolved_at, updated_at = now()
WHERE id = @id;

-- name: ListDisputeProposals :many
SELECT * FROM dispute_proposals ORDER BY dispute_id, ord;

-- name: ListProposalsOfDispute :many
SELECT * FROM dispute_proposals WHERE dispute_id = $1 ORDER BY ord;

-- name: InsertDisputeProposal :exec
INSERT INTO dispute_proposals (id, dispute_id, by_id, type, amount, note, at, status)
VALUES (@id, @dispute_id, @by_id, @type, @amount, @note, @at, @status);

-- name: SetProposalStatus :exec
UPDATE dispute_proposals SET status = @status WHERE id = @id;

-- name: ListDisputePhotos :many
SELECT * FROM dispute_photos ORDER BY dispute_id, position;

-- name: ListPhotosOfDispute :many
SELECT * FROM dispute_photos WHERE dispute_id = $1 ORDER BY position;

-- name: InsertDisputePhotos :exec
INSERT INTO dispute_photos (dispute_id, position, photo_id)
SELECT @dispute_id::text, (p.ordinality - 1)::int, p.photo_id
FROM unnest(@photo_ids::text[]) WITH ORDINALITY AS p(photo_id, ordinality);

/* ----------------------------------------------------------------- strikes */

-- name: ListStrikes :many
SELECT * FROM strikes ORDER BY ord DESC;

-- name: ListStrikesOfUser :many
SELECT * FROM strikes WHERE user_id = $1 ORDER BY ord DESC;

-- name: InsertStrike :exec
INSERT INTO strikes (id, user_id, reason, severity, booking_id, at, by_admin_id)
VALUES (@id, @user_id, @reason, @severity, @booking_id, @at, @by_admin_id);

/* ------------------------------------------------------------- wanted board */

-- name: GetWantedForUpdate :one
SELECT id, ord, requester_id, title, description, category, area_id, lat, lng, radius_km, needed_from, needed_until,
  budget_per_day, status, created_at
FROM wanted_posts WHERE id = $1 FOR UPDATE;

-- name: WantedExists :one
SELECT EXISTS (SELECT 1 FROM wanted_posts WHERE id = $1);

-- name: ListWanted :many
SELECT id, ord, requester_id, title, description, category, area_id, lat, lng, radius_km, needed_from, needed_until,
  budget_per_day, status, created_at
FROM wanted_posts ORDER BY ord DESC;

-- name: ListOpenWanted :many
SELECT id, ord, requester_id, title, description, category, area_id, lat, lng, radius_km, needed_from, needed_until,
  budget_per_day, status, created_at
FROM wanted_posts WHERE status = 'terbuka' ORDER BY ord DESC;

-- name: InsertWanted :exec
INSERT INTO wanted_posts (id, requester_id, title, description, category, area_id, lat, lng, radius_km, needed_from,
  needed_until, budget_per_day, status, created_at)
VALUES (@id, @requester_id, @title, @description, @category, @area_id, @lat, @lng, @radius_km, @needed_from, @needed_until,
  @budget_per_day, @status, @created_at);

-- name: SetWantedStatus :exec
UPDATE wanted_posts SET status = @status, updated_at = now() WHERE id = @id;

-- name: ListWantedOffers :many
SELECT * FROM wanted_offers ORDER BY wanted_id, ord;

-- name: InsertWantedOffer :exec
INSERT INTO wanted_offers (id, wanted_id, provider_id, listing_id, message, at)
VALUES (@id, @wanted_id, @provider_id, @listing_id, @message, @at);

/* ----------------------------------------------------------- notifications */

-- name: ListNotifications :many
SELECT * FROM notifications ORDER BY ord DESC;

-- name: ListNotificationsOfUser :many
SELECT * FROM notifications WHERE user_id = $1 ORDER BY ord DESC;

-- name: InsertNotification :exec
INSERT INTO notifications (id, user_id, kind, title, body, href, at, read_at)
VALUES (@id, @user_id, @kind, @title, @body, @href, @at, @read_at);

-- name: MarkNotificationsRead :execrows
UPDATE notifications SET read_at = @now
WHERE user_id = @user_id AND read_at IS NULL AND (sqlc.narg(ids)::text[] IS NULL OR id = ANY (sqlc.narg(ids)::text[]));

/* ---------------------------------------------------------------- audit log */

-- name: ListAudit :many
SELECT * FROM audit_log ORDER BY ord DESC;

-- name: InsertAudit :exec
INSERT INTO audit_log (id, actor_id, action, target, old_value, new_value, at, request_id, ip)
VALUES (@id, @actor_id, @action, @target, @old_value, @new_value, @at, @request_id, @ip);
