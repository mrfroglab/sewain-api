-- name: GetListing :one
SELECT * FROM listing_rows WHERE id = $1;

-- name: GetListingForUpdate :one
SELECT * FROM listing_rows WHERE id = $1 FOR UPDATE;

-- name: ListingExists :one
SELECT EXISTS (SELECT 1 FROM listings WHERE id = $1);

-- name: ListListings :many
SELECT * FROM listing_rows ORDER BY ord DESC;

-- name: ListListingsVisibleTo :many
-- Active listings of active owners, the viewer's own listings (any status) and listings of the viewer's bookings.
SELECT * FROM listing_rows l
WHERE (l.status = 'aktif' AND EXISTS (SELECT 1 FROM users o WHERE o.id = l.owner_id AND o.status = 'aktif' AND o.deleted_at IS NULL))
   OR l.owner_id = @viewer
   OR EXISTS (SELECT 1 FROM bookings b WHERE b.listing_id = l.id AND (b.renter_id = @viewer OR b.provider_id = @viewer))
ORDER BY ord DESC;

-- name: ListActiveListingsByOthers :many
SELECT * FROM listing_rows WHERE status = 'aktif' AND owner_id <> @owner_id ORDER BY ord DESC;

-- name: ListActiveListingsByOwnerForUpdate :many
SELECT * FROM listing_rows WHERE owner_id = $1 AND status = 'aktif' ORDER BY ord DESC FOR UPDATE;

-- name: InsertListing :exec
INSERT INTO listings (id, ord, slug, owner_id, title, category, description, condition_notes, price_per_day, price_per_hour,
  price_per_week, duration_discount, min_days, max_days, item_value, lat, lng, area_id, radius_km, handover, meeting_point,
  handover_hours, late_fee_per_day, late_fee_cap_percent, cancellation, special_rules, approval, auto_approve_min_rating,
  stock, vehicle, status, rent_count, base_rating, base_rating_count, created_at)
VALUES (@id, nextval('row_ord_seq'), @slug, @owner_id, @title, @category, @description, @condition_notes, @price_per_day,
  @price_per_hour, @price_per_week, @duration_discount, @min_days, @max_days, @item_value, @lat, @lng, @area_id, @radius_km,
  @handover, @meeting_point, @handover_hours, @late_fee_per_day, @late_fee_cap_percent, @cancellation, @special_rules,
  @approval, @auto_approve_min_rating, @stock, @vehicle, @status, @rent_count, @base_rating, @base_rating_count, @created_at);

-- name: UpdateListing :exec
UPDATE listings SET title = @title, category = @category, description = @description, condition_notes = @condition_notes,
  price_per_day = @price_per_day, price_per_hour = @price_per_hour, price_per_week = @price_per_week,
  duration_discount = @duration_discount, min_days = @min_days, max_days = @max_days, item_value = @item_value, lat = @lat,
  lng = @lng, area_id = @area_id, radius_km = @radius_km, handover = @handover, meeting_point = @meeting_point,
  handover_hours = @handover_hours, late_fee_per_day = @late_fee_per_day, late_fee_cap_percent = @late_fee_cap_percent,
  cancellation = @cancellation, special_rules = @special_rules, approval = @approval,
  auto_approve_min_rating = @auto_approve_min_rating, stock = @stock, vehicle = @vehicle, status = @status,
  rent_count = @rent_count, updated_at = now()
WHERE id = @id;

-- name: SetListingStatus :exec
UPDATE listings SET status = @status, updated_at = now() WHERE id = @id;

-- name: IncrementListingRentCount :exec
UPDATE listings SET rent_count = rent_count + 1, updated_at = now() WHERE id = $1;

-- name: ListListingPhotos :many
SELECT * FROM listing_photos ORDER BY listing_id, position;

-- name: ListPhotosOfListing :many
SELECT * FROM listing_photos WHERE listing_id = $1 ORDER BY position;

-- name: DeleteListingPhotos :exec
DELETE FROM listing_photos WHERE listing_id = $1;

-- name: InsertListingPhotos :exec
INSERT INTO listing_photos (listing_id, position, src)
SELECT @listing_id::text, (p.ordinality - 1)::int, p.src
FROM unnest(@srcs::text[]) WITH ORDINALITY AS p(src, ordinality);

-- name: ListCollateralOptions :many
SELECT * FROM collateral_options ORDER BY listing_id, position;

-- name: ListCollateralOptionsOfListing :many
SELECT * FROM collateral_options WHERE listing_id = $1 ORDER BY position;

-- name: DeleteCollateralOptions :exec
DELETE FROM collateral_options WHERE listing_id = $1;

-- name: InsertCollateralOption :exec
INSERT INTO collateral_options (listing_id, id, position, kind, cards, deposit_mode, deposit_amount, deposit_percent,
  with_identity, description, estimated_value)
VALUES (@listing_id, @id, @position, @kind, @cards, @deposit_mode, @deposit_amount, @deposit_percent, @with_identity,
  @description, @estimated_value);

-- name: CountCommittedOverlaps :one
-- FR-R4: bookings that hold stock on overlapping dates: ACCEPTED/ACTIVE, and DISPUTED while the item is still with the
-- renter (not returned), like listingBusy in the snapshot.
SELECT count(*)::int FROM bookings
WHERE listing_id = @listing_id AND (status IN ('ACCEPTED', 'ACTIVE') OR (status = 'DISPUTED' AND returned_at IS NULL))
  AND id <> @exclude_id
  AND start_date <= @end_date AND @start_date <= end_date;

-- name: ListFavorites :many
SELECT * FROM favorites ORDER BY user_id, ord;

-- name: ListFavoritesOfUser :many
SELECT * FROM favorites WHERE user_id = $1 ORDER BY ord;

-- name: DeleteFavorite :execrows
DELETE FROM favorites WHERE user_id = @user_id AND listing_id = @listing_id;

-- name: InsertFavorite :exec
INSERT INTO favorites (user_id, listing_id, ord) VALUES (@user_id, @listing_id, nextval('row_ord_seq'));

-- name: SearchListings :many
-- Candidates for the search (FR-S3/FR-S4): active listings of active owners within @radius_m of the point, plus
-- filters. The caller passes the search radius plus a margin larger than the public-location fuzz and decides
-- membership, distance and the two-way reach on the PUBLIC point (geo.FuzzPoint), so results never act as an oracle
-- on the precise location (review C1).
WITH pt AS (SELECT ST_SetSRID(ST_MakePoint(@lng::float8, @lat::float8), 4326)::geography AS g)
SELECT l.id, l.lat, l.lng, l.radius_km,
       (CASE WHEN sqlc.narg(tsquery)::text IS NULL THEN 0
             ELSE ts_rank(l.search, to_tsquery('simple', sqlc.narg(tsquery)::text)) END)::float8 AS rank
FROM listings l
JOIN users u ON u.id = l.owner_id
CROSS JOIN pt
WHERE l.status = 'aktif' AND u.status = 'aktif' AND u.deleted_at IS NULL
  AND ST_DWithin(l.location, pt.g, @radius_m::float8)
  AND (sqlc.narg(category)::text IS NULL OR l.category = sqlc.narg(category)::text)
  AND (sqlc.narg(min_price)::bigint IS NULL OR l.price_per_day >= sqlc.narg(min_price)::bigint)
  AND (sqlc.narg(max_price)::bigint IS NULL OR l.price_per_day <= sqlc.narg(max_price)::bigint)
  AND (sqlc.narg(handover)::text IS NULL OR sqlc.narg(handover)::text = ANY (l.handover))
  AND (sqlc.narg(collateral)::text IS NULL OR EXISTS (
        SELECT 1 FROM collateral_options o WHERE o.listing_id = l.id AND o.kind = sqlc.narg(collateral)::text))
  AND (sqlc.narg(provider_type)::text IS NULL OR u.account_type = sqlc.narg(provider_type)::text)
  AND (sqlc.narg(tsquery)::text IS NULL OR l.search @@ to_tsquery('simple', sqlc.narg(tsquery)::text))
  AND (sqlc.narg(start_date)::date IS NULL OR sqlc.narg(end_date)::date IS NULL OR (
        SELECT count(*) FROM bookings b
        WHERE b.listing_id = l.id AND b.status IN ('ACCEPTED', 'ACTIVE')
          AND b.start_date <= sqlc.narg(end_date)::date AND sqlc.narg(start_date)::date <= b.end_date) < l.stock)
ORDER BY l.id;

-- name: ListActiveListingsOfOwner :many
-- Public profile page (GET /api/v1/users/{id}): the owner's active listings.
SELECT * FROM listing_rows WHERE owner_id = $1 AND status = 'aktif' ORDER BY ord DESC;
