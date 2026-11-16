-- +goose Up
-- Sewain v2 core schema (ROADMAP §3). Relational, enums as text + CHECK, JSONB only for immutable snapshots
-- (rental agreement, check-out/check-in details, payment summary). Every row that appears in the web snapshot has
-- an `ord` column from one global sequence: the web expects collections in the v1 order (append vs prepend), and
-- `ord` reproduces that exactly, including for the seed.

CREATE EXTENSION IF NOT EXISTS postgis;

-- v1 stored every entity as a JSONB document (column `data`). Those tables share names with v2, so on a database that
-- still holds the v1 schema they are dropped first. The demo data is re-seeded afterwards.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = current_schema() AND table_name = 'users' AND column_name = 'data') THEN
    DROP TABLE IF EXISTS users, verifications, listings, bookings, photos, messages, reviews, reports, disputes,
      strikes, audit, wanted, notifications, settings, sessions, schema_migrations CASCADE;
  END IF;
END
$$;
-- +goose StatementEnd

CREATE SEQUENCE row_ord_seq;
CREATE SEQUENCE state_version_seq;
CREATE SEQUENCE booking_code_seq;

-- Append-only guard for evidence tables (FR-P4, FR-K7). Only the retention job (or a demo reset) may delete,
-- and it must opt in with: SET LOCAL sewain.retention = 'on'.
-- +goose StatementBegin
CREATE FUNCTION sewain_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION '% is append-only: UPDATE is not allowed', TG_TABLE_NAME USING ERRCODE = 'insufficient_privilege';
  END IF;
  IF coalesce(current_setting('sewain.retention', true), '') = 'on' THEN
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NULL;
  END IF;
  RAISE EXCEPTION '% is append-only: % requires sewain.retention = on', TG_TABLE_NAME, TG_OP USING ERRCODE = 'insufficient_privilege';
END
$$;
-- +goose StatementEnd

/* ------------------------------------------------------------------ users */

CREATE TABLE users (
  id                  text PRIMARY KEY,
  ord                 bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  phone               text NOT NULL,
  phone_verified_at   timestamptz,
  name                text NOT NULL,
  nickname            text,
  email               text,
  role                text NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
  account_type        text NOT NULL DEFAULT 'individu' CHECK (account_type IN ('individu', 'toko')),
  area_id             text NOT NULL,
  campus              text,
  bio                 text,
  status              text NOT NULL DEFAULT 'aktif' CHECK (status IN ('aktif', 'suspend', 'blacklist')),
  strikes             integer NOT NULL DEFAULT 0 CHECK (strikes >= 0),
  verification_status text NOT NULL DEFAULT 'belum' CHECK (verification_status IN ('belum', 'menunggu', 'terverifikasi', 'ditolak')),
  verification_method text CHECK (verification_method IN ('ekyc', 'demo', 'manual')),
  verified_at         timestamptz,
  nik_masked          text,
  early_lender        boolean,
  student_verified    boolean,
  base_rating         double precision,
  base_rating_count   integer,
  demo                jsonb,
  joined_at           timestamptz NOT NULL,
  deleted_at          timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_phone_key ON users (phone) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX users_email_key ON users (lower(email)) WHERE email IS NOT NULL AND email <> '' AND deleted_at IS NULL;
CREATE INDEX users_nik_masked_idx ON users (nik_masked) WHERE nik_masked IS NOT NULL;

CREATE TABLE stores (
  user_id    text PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
  name       text NOT NULL,
  area_id    text NOT NULL,
  hours      text NOT NULL,
  nib        text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE verification_requests (
  id                text PRIMARY KEY,
  ord               bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  user_id           text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  method            text NOT NULL CHECK (method IN ('ekyc', 'demo', 'manual')),
  name_on_ktp       text NOT NULL,
  nik_masked        text NOT NULL,
  ktp_photo_id      text,
  selfie_photo_id   text,
  status            text NOT NULL CHECK (status IN ('menunggu', 'disetujui', 'ditolak')),
  auto              boolean NOT NULL DEFAULT false,
  submitted_at      timestamptz NOT NULL,
  reviewed_at       timestamptz,
  reviewed_by       text,
  reason            text,
  kyc_submission_id text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX verification_requests_status_idx ON verification_requests (status, submitted_at);
CREATE INDEX verification_requests_user_idx ON verification_requests (user_id);

/* --------------------------------------------------------------- listings */

CREATE TABLE listings (
  id                      text PRIMARY KEY,
  ord                     bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  slug                    text NOT NULL,
  owner_id                text NOT NULL REFERENCES users (id),
  title                   text NOT NULL,
  category                text NOT NULL,
  description             text NOT NULL DEFAULT '',
  condition_notes         text NOT NULL DEFAULT '',
  price_per_day           bigint NOT NULL CHECK (price_per_day > 0),
  price_per_hour          bigint,
  price_per_week          bigint,
  duration_discount       jsonb,
  min_days                integer,
  max_days                integer,
  item_value              bigint NOT NULL CHECK (item_value > 0),
  lat                     double precision NOT NULL,
  lng                     double precision NOT NULL,
  location                geography(Point, 4326) GENERATED ALWAYS AS (ST_SetSRID(ST_MakePoint(lng, lat), 4326)::geography) STORED,
  area_id                 text NOT NULL,
  radius_km               double precision NOT NULL CHECK (radius_km > 0),
  handover                text[] NOT NULL DEFAULT '{}',
  meeting_point           text,
  handover_hours          text,
  late_fee_per_day        bigint NOT NULL DEFAULT 0,
  late_fee_cap_percent    double precision NOT NULL DEFAULT 0,
  cancellation            text NOT NULL DEFAULT '',
  special_rules           text,
  approval                text NOT NULL DEFAULT 'manual' CHECK (approval IN ('manual', 'otomatis')),
  auto_approve_min_rating double precision,
  stock                   integer NOT NULL DEFAULT 1 CHECK (stock >= 1),
  vehicle                 jsonb,
  status                  text NOT NULL DEFAULT 'aktif' CHECK (status IN ('aktif', 'jeda', 'diturunkan')),
  rent_count              integer NOT NULL DEFAULT 0,
  base_rating             double precision NOT NULL DEFAULT 0,
  base_rating_count       integer NOT NULL DEFAULT 0,
  search                  tsvector GENERATED ALWAYS AS (
                            to_tsvector('simple', coalesce(title, '') || ' ' || coalesce(description, '') || ' ' || coalesce(category, ''))
                          ) STORED,
  created_at              timestamptz NOT NULL,
  updated_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX listings_location_gix ON listings USING gist (location);
CREATE INDEX listings_search_gin ON listings USING gin (search);
CREATE INDEX listings_owner_idx ON listings (owner_id);
CREATE INDEX listings_status_category_idx ON listings (status, category);

-- Listing columns without the PostGIS/tsvector ones (what the application reads).
CREATE VIEW listing_rows AS
SELECT id, ord, slug, owner_id, title, category, description, condition_notes, price_per_day, price_per_hour, price_per_week,
       duration_discount, min_days, max_days, item_value, lat, lng, area_id, radius_km, handover, meeting_point, handover_hours,
       late_fee_per_day, late_fee_cap_percent, cancellation, special_rules, approval, auto_approve_min_rating, stock, vehicle,
       status, rent_count, base_rating, base_rating_count, created_at, updated_at
FROM listings;

CREATE TABLE listing_photos (
  listing_id text NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
  position   integer NOT NULL,
  src        text NOT NULL,
  PRIMARY KEY (listing_id, position)
);

-- Collateral options a provider offers (PRD Lampiran A). Bookings keep their own JSONB snapshot of the chosen one.
CREATE TABLE collateral_options (
  listing_id      text NOT NULL REFERENCES listings (id) ON DELETE CASCADE,
  id              text NOT NULL,
  position        integer NOT NULL,
  kind            text NOT NULL CHECK (kind IN ('identitas', 'deposit', 'barang', 'custom')),
  cards           text[],
  deposit_mode    text CHECK (deposit_mode IN ('tetap', 'persen')),
  deposit_amount  bigint,
  deposit_percent double precision,
  with_identity   boolean,
  description     text,
  estimated_value bigint,
  PRIMARY KEY (listing_id, id)
);
CREATE INDEX collateral_options_kind_idx ON collateral_options (kind, listing_id);

/* --------------------------------------------------------------- bookings */

CREATE TABLE bookings (
  id                   text PRIMARY KEY,
  ord                  bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  code                 text NOT NULL UNIQUE,
  listing_id           text NOT NULL REFERENCES listings (id),
  renter_id            text NOT NULL REFERENCES users (id),
  provider_id          text NOT NULL REFERENCES users (id),
  start_date           date NOT NULL,
  end_date             date NOT NULL,
  handover             text NOT NULL,
  collateral           jsonb NOT NULL,
  note                 text,
  status               text NOT NULL CHECK (status IN ('REQUESTED', 'ACCEPTED', 'DECLINED', 'EXPIRED', 'CANCELLED', 'ACTIVE',
                         'RETURNED', 'COMPLETED', 'DISPUTED', 'RESOLVED')),
  overdue              boolean NOT NULL DEFAULT false,
  created_at           timestamptz NOT NULL,
  expires_at           timestamptz NOT NULL,
  accepted_at          timestamptz,
  declined_at          timestamptz,
  decline_reason       text,
  cancelled_at         timestamptz,
  cancelled_by         text,
  cancel_reason        text,
  active_at            timestamptz,
  returned_at          timestamptz,
  completed_at         timestamptz,
  completed_auto       boolean,
  resolved_at          timestamptz,
  -- The agreement document as hashed at request time (FR-R2). Agreement timestamps live in their own columns.
  agreement            jsonb NOT NULL,
  agreement_hash       text NOT NULL,
  renter_agreed_at     timestamptz,
  provider_agreed_at   timestamptz,
  rent_total           bigint NOT NULL,
  collateral_value     bigint NOT NULL DEFAULT 0,
  commission_amount    bigint NOT NULL DEFAULT 0,
  commission_charged   bigint NOT NULL DEFAULT 0,
  commission_bearer    text NOT NULL DEFAULT 'penyewa',
  payment              jsonb NOT NULL,
  meeting_point_detail text,
  checkout             jsonb,
  checkin              jsonb,
  collateral_return    jsonb,
  dispute_id           text,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  CHECK (end_date >= start_date)
);
CREATE INDEX bookings_schedule_idx ON bookings (listing_id, status, start_date, end_date);
CREATE INDEX bookings_renter_idx ON bookings (renter_id);
CREATE INDEX bookings_provider_idx ON bookings (provider_id);
CREATE INDEX bookings_requested_expiry_idx ON bookings (expires_at) WHERE status = 'REQUESTED';

/* ----------------------------------------------------------------- photos */

-- Photo evidence: SHA-256 over the received bytes + server timestamp (FR-P2/P3). Append-only (FR-P4).
CREATE TABLE photos (
  id          text PRIMARY KEY,
  ord         bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  booking_id  text,
  listing_id  text,
  stage       text NOT NULL CHECK (stage IN ('referensi', 'checkout', 'checkin', 'jaminan', 'masalah', 'chat', 'sengketa', 'ktp', 'selfie')),
  uploader_id text NOT NULL,
  src         text NOT NULL,
  object_key  text,
  sha256      text NOT NULL,
  server_ts   timestamptz NOT NULL,
  source      text NOT NULL CHECK (source IN ('kamera', 'galeri', 'demo')),
  angle       text,
  width       integer,
  height      integer,
  bytes       bigint,
  mime        text,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX photos_booking_idx ON photos (booking_id);
CREATE INDEX photos_uploader_idx ON photos (uploader_id);
CREATE TRIGGER photos_append_only BEFORE UPDATE OR DELETE ON photos FOR EACH ROW EXECUTE FUNCTION sewain_append_only();
CREATE TRIGGER photos_append_only_truncate BEFORE TRUNCATE ON photos FOR EACH STATEMENT EXECUTE FUNCTION sewain_append_only();

/* ---------------------------------------------------------------- chat */

CREATE TABLE messages (
  id         text PRIMARY KEY,
  ord        bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  booking_id text NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
  sender_id  text, -- NULL = system message
  text       text,
  photo_id   text,
  at         timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_booking_idx ON messages (booking_id, at);

-- FR-C3: chat cannot be deleted while the booking is running or disputed.
-- +goose StatementBegin
CREATE FUNCTION sewain_messages_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF coalesce(current_setting('sewain.retention', true), '') <> 'on' AND EXISTS (
    SELECT 1 FROM bookings b WHERE b.id = OLD.booking_id AND b.status IN ('ACCEPTED', 'ACTIVE', 'RETURNED', 'DISPUTED')) THEN
    RAISE EXCEPTION 'messages of an active or disputed booking cannot be deleted' USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN OLD;
END
$$;
-- +goose StatementEnd
CREATE TRIGGER messages_guard BEFORE DELETE ON messages FOR EACH ROW EXECUTE FUNCTION sewain_messages_guard();

CREATE TABLE chat_reads (
  user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  booking_id text NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
  read_at    timestamptz NOT NULL,
  PRIMARY KEY (user_id, booking_id)
);

CREATE TABLE favorites (
  user_id    text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  listing_id text NOT NULL,
  ord        bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, listing_id)
);

/* ------------------------------------------------------------- reviews */

-- FR-T2: a review is shown to others once both parties reviewed or 7 days passed (`visible_at`); the periodic job
-- flips `released` so the change bumps the state version.
CREATE TABLE reviews (
  id         text PRIMARY KEY,
  ord        bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  booking_id text NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
  listing_id text NOT NULL,
  from_id    text NOT NULL REFERENCES users (id),
  to_id      text NOT NULL REFERENCES users (id),
  from_role  text NOT NULL CHECK (from_role IN ('penyewa', 'penyedia')),
  rating     double precision NOT NULL CHECK (rating >= 1 AND rating <= 5),
  comment    text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL,
  visible_at timestamptz NOT NULL,
  released   boolean NOT NULL DEFAULT false,
  UNIQUE (booking_id, from_id)
);
CREATE INDEX reviews_to_idx ON reviews (to_id);
CREATE INDEX reviews_pending_release_idx ON reviews (visible_at) WHERE NOT released;

/* ---------------------------------------------------------- moderation */

CREATE TABLE reports (
  id          text PRIMARY KEY,
  ord         bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  reporter_id text NOT NULL REFERENCES users (id),
  target_type text NOT NULL CHECK (target_type IN ('listing', 'user')),
  target_id   text NOT NULL,
  reason      text NOT NULL,
  detail      text,
  status      text NOT NULL DEFAULT 'baru' CHECK (status IN ('baru', 'ditindak', 'ditolak')),
  resolution  text,
  resolved_at timestamptz,
  resolved_by text,
  created_at  timestamptz NOT NULL
);
CREATE INDEX reports_status_idx ON reports (status, created_at);

CREATE TABLE disputes (
  id             text PRIMARY KEY,
  ord            bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  booking_id     text NOT NULL REFERENCES bookings (id) ON DELETE CASCADE,
  opened_by      text NOT NULL REFERENCES users (id),
  case_type      text NOT NULL,
  description    text NOT NULL,
  layer          text NOT NULL CHECK (layer IN ('L1', 'L2', 'L3')),
  status         text NOT NULL CHECK (status IN ('negosiasi', 'mediasi', 'selesai')),
  l1_deadline    timestamptz NOT NULL,
  recommendation jsonb,
  outcome        text,
  resolved_at    timestamptz,
  created_at     timestamptz NOT NULL,
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX disputes_open_booking_key ON disputes (booking_id) WHERE status <> 'selesai';
CREATE INDEX disputes_l1_deadline_idx ON disputes (l1_deadline) WHERE status = 'negosiasi';
ALTER TABLE bookings ADD CONSTRAINT bookings_dispute_fk FOREIGN KEY (dispute_id) REFERENCES disputes (id) DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE dispute_proposals (
  id         text PRIMARY KEY,
  ord        bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  dispute_id text NOT NULL REFERENCES disputes (id) ON DELETE CASCADE,
  by_id      text NOT NULL REFERENCES users (id),
  type       text NOT NULL CHECK (type IN ('potong-deposit', 'ganti-perbaikan', 'ganti-nilai', 'perpanjang', 'tanpa-kompensasi')),
  amount     bigint,
  note       text,
  at         timestamptz NOT NULL,
  status     text NOT NULL CHECK (status IN ('diajukan', 'diterima', 'ditolak'))
);
CREATE INDEX dispute_proposals_dispute_idx ON dispute_proposals (dispute_id);

CREATE TABLE dispute_photos (
  dispute_id text NOT NULL REFERENCES disputes (id) ON DELETE CASCADE,
  position   integer NOT NULL,
  photo_id   text NOT NULL,
  PRIMARY KEY (dispute_id, position)
);

CREATE TABLE strikes (
  id          text PRIMARY KEY,
  ord         bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  user_id     text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  reason      text NOT NULL,
  severity    text NOT NULL CHECK (severity IN ('ringan', 'berat')),
  booking_id  text,
  at          timestamptz NOT NULL,
  by_admin_id text NOT NULL
);
CREATE INDEX strikes_user_idx ON strikes (user_id);

/* ------------------------------------------------------------ wanted board */

CREATE TABLE wanted_posts (
  id             text PRIMARY KEY,
  ord            bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  requester_id   text NOT NULL REFERENCES users (id),
  title          text NOT NULL,
  description    text NOT NULL DEFAULT '',
  category       text,
  area_id        text NOT NULL,
  lat            double precision NOT NULL,
  lng            double precision NOT NULL,
  location       geography(Point, 4326) GENERATED ALWAYS AS (ST_SetSRID(ST_MakePoint(lng, lat), 4326)::geography) STORED,
  radius_km      double precision NOT NULL,
  needed_from    date,
  needed_until   date,
  budget_per_day bigint,
  status         text NOT NULL DEFAULT 'terbuka' CHECK (status IN ('terbuka', 'terpenuhi', 'ditutup')),
  created_at     timestamptz NOT NULL,
  updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX wanted_posts_location_gix ON wanted_posts USING gist (location);

CREATE TABLE wanted_offers (
  id          text PRIMARY KEY,
  ord         bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  wanted_id   text NOT NULL REFERENCES wanted_posts (id) ON DELETE CASCADE,
  provider_id text NOT NULL REFERENCES users (id),
  listing_id  text,
  message     text NOT NULL DEFAULT '',
  at          timestamptz NOT NULL
);
CREATE INDEX wanted_offers_wanted_idx ON wanted_offers (wanted_id);

/* ---------------------------------------------------------- notifications */

CREATE TABLE notifications (
  id      text PRIMARY KEY,
  ord     bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  user_id text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  kind    text NOT NULL,
  title   text NOT NULL,
  body    text NOT NULL,
  href    text,
  at      timestamptz NOT NULL,
  read_at timestamptz
);
CREATE INDEX notifications_user_idx ON notifications (user_id, at DESC);

/* ------------------------------------------------------------- audit log */

CREATE TABLE audit_log (
  id         text PRIMARY KEY,
  ord        bigint NOT NULL DEFAULT nextval('row_ord_seq'),
  actor_id   text NOT NULL,
  action     text NOT NULL,
  target     text NOT NULL,
  old_value  text,
  new_value  text,
  at         timestamptz NOT NULL,
  request_id text,
  ip         text
);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC);
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION sewain_append_only();
CREATE TRIGGER audit_log_append_only_truncate BEFORE TRUNCATE ON audit_log FOR EACH STATEMENT EXECUTE FUNCTION sewain_append_only();

/* ----------------------------------------------------------- configuration */

-- Commission settings are versioned: the newest row applies to new bookings, history is kept (FR-K5, FR-K7).
CREATE TABLE commission_configs (
  version              bigserial PRIMARY KEY,
  active               boolean NOT NULL,
  mode                 text NOT NULL CHECK (mode IN ('persen', 'flat')),
  percent              double precision NOT NULL CHECK (percent >= 0 AND percent <= 100),
  flat                 bigint NOT NULL CHECK (flat >= 0),
  min                  bigint,
  max                  bigint,
  bearer               text NOT NULL CHECK (bearer IN ('penyewa', 'penyedia', 'split')),
  split_renter_percent double precision NOT NULL CHECK (split_renter_percent >= 0 AND split_renter_percent <= 100),
  effective_from       timestamptz NOT NULL,
  created_by           text NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now()
);

-- Collateral suggestion thresholds (FR-J3), versioned the same way.
CREATE TABLE collateral_rule_sets (
  version                 bigserial PRIMARY KEY,
  low_max                 bigint NOT NULL,
  mid_max                 bigint NOT NULL,
  mid_deposit_percent     double precision NOT NULL,
  high_deposit_percent    double precision NOT NULL,
  vehicle_deposit_percent double precision NOT NULL,
  high_value_badge        bigint NOT NULL,
  effective_from          timestamptz NOT NULL,
  created_by              text NOT NULL,
  created_at              timestamptz NOT NULL DEFAULT now(),
  CHECK (low_max > 0 AND mid_max > low_max)
);

-- Listing keyword filter (FR-L4).
CREATE TABLE banned_keywords (
  word       text PRIMARY KEY,
  position   integer NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Small global settings: demo clock offset, seed metadata.
CREATE TABLE app_settings (
  key        text PRIMARY KEY,
  value      jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- Demo bearer sessions (DEMO_MODE persona login). Only the SHA-256 of the token is stored.
-- Real sessions (OTP + cookie) belong to the auth module (auth_sessions, migrations 00100+).
-- No FK to users: sessions survive a demo reset (the web keeps its tokens), like v1.
CREATE TABLE demo_sessions (
  token_hash text PRIMARY KEY,
  user_id    text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX demo_sessions_user_idx ON demo_sessions (user_id);

-- Idempotency-Key for actions (F1.8): a replay returns the stored response. Cleaned after 24 hours.
CREATE TABLE idempotency_keys (
  user_id      text NOT NULL,
  key          text NOT NULL,
  action       text NOT NULL,
  request_hash text NOT NULL,
  status       integer NOT NULL,
  response     text NOT NULL, -- the exact response body (byte-identical replays)
  created_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, key)
);
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

-- One row per committed state change. `version` is the snapshot version; the snapshot reads max(version) inside its
-- own transaction, so a version it reports is always fully visible. Realtime fan-out reads the audience columns.
CREATE TABLE change_log (
  version bigint PRIMARY KEY,
  users   text[] NOT NULL DEFAULT '{}',
  admins  boolean NOT NULL DEFAULT false,
  public  boolean NOT NULL DEFAULT false,
  at      timestamptz NOT NULL DEFAULT now()
);

/* ---------------------------------------------------------------- analytics (PRD §9.8, §17) */

CREATE VIEW analytics_bookings_by_status AS
SELECT status, count(*) AS bookings, coalesce(sum(rent_total), 0) AS rent_value
FROM bookings GROUP BY status;

CREATE VIEW analytics_rent_value AS
SELECT date_trunc('month', created_at AT TIME ZONE 'Asia/Jakarta')::date AS month,
       count(*) FILTER (WHERE status IN ('COMPLETED', 'RESOLVED')) AS completed,
       coalesce(sum(rent_total) FILTER (WHERE status IN ('COMPLETED', 'RESOLVED')), 0) AS rent_value,
       coalesce(sum(commission_amount) FILTER (WHERE status IN ('COMPLETED', 'RESOLVED')), 0) AS commission_if_active,
       coalesce(sum(commission_charged) FILTER (WHERE status IN ('COMPLETED', 'RESOLVED')), 0) AS commission_charged
FROM bookings GROUP BY 1;

CREATE VIEW analytics_disputes_by_case AS
SELECT case_type, layer, status, count(*) AS disputes
FROM disputes GROUP BY case_type, layer, status;

CREATE VIEW analytics_request_funnel AS
SELECT count(*) AS requests,
       count(*) FILTER (WHERE accepted_at IS NOT NULL) AS accepted,
       count(*) FILTER (WHERE status IN ('COMPLETED', 'RESOLVED')) AS completed,
       round(100.0 * count(*) FILTER (WHERE accepted_at IS NOT NULL) / nullif(count(*), 0), 1) AS accepted_percent,
       round(100.0 * count(*) FILTER (WHERE status IN ('COMPLETED', 'RESOLVED')) / nullif(count(*), 0), 1) AS completed_percent,
       avg(coalesce(accepted_at, declined_at) - created_at) FILTER (WHERE coalesce(accepted_at, declined_at) IS NOT NULL) AS avg_response_time
FROM bookings;

-- North Star: rentals completed per week.
CREATE VIEW analytics_weekly_completed AS
SELECT date_trunc('week', completed_at AT TIME ZONE 'Asia/Jakarta')::date AS week, count(*) AS completed
FROM bookings WHERE completed_at IS NOT NULL GROUP BY 1;

-- +goose Down
DROP VIEW IF EXISTS listing_rows, analytics_weekly_completed, analytics_request_funnel, analytics_disputes_by_case, analytics_rent_value,
  analytics_bookings_by_status;
DROP TABLE IF EXISTS change_log, idempotency_keys, demo_sessions, app_settings, banned_keywords, collateral_rule_sets, commission_configs,
  audit_log, notifications, wanted_offers, wanted_posts, strikes, dispute_photos, dispute_proposals, reports, reviews,
  favorites, chat_reads, messages, photos, collateral_options, listing_photos, verification_requests, stores CASCADE;
ALTER TABLE IF EXISTS bookings DROP CONSTRAINT IF EXISTS bookings_dispute_fk;
DROP TABLE IF EXISTS disputes, bookings, listings, users CASCADE;
DROP FUNCTION IF EXISTS sewain_messages_guard();
DROP FUNCTION IF EXISTS sewain_append_only();
DROP SEQUENCE IF EXISTS booking_code_seq, state_version_seq, row_ord_seq;
