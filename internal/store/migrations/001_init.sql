-- Sewain schema. Each entity is stored as a JSONB document in the exact shape the web app uses,
-- with the columns we filter/sort/join on generated from the document (always in sync, indexable).
CREATE EXTENSION IF NOT EXISTS postgis;

CREATE TABLE users (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  phone      text GENERATED ALWAYS AS (data->>'phone') STORED,
  role       text GENERATED ALWAYS AS (data->>'role') STORED,
  status     text GENERATED ALWAYS AS (data->>'status') STORED,
  nik_masked text GENERATED ALWAYS AS (data->'verification'->>'nikMasked') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'joinedAt')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_phone_key ON users (phone);

CREATE TABLE verifications (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  user_id    text GENERATED ALWAYS AS (data->>'userId') STORED,
  status     text GENERATED ALWAYS AS (data->>'status') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'submittedAt')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE listings (
  id            text PRIMARY KEY,
  data          jsonb NOT NULL,
  owner_id      text GENERATED ALWAYS AS (data->>'ownerId') STORED,
  category      text GENERATED ALWAYS AS (data->>'category') STORED,
  status        text GENERATED ALWAYS AS (data->>'status') STORED,
  price_per_day bigint GENERATED ALWAYS AS ((data->>'pricePerDay')::bigint) STORED,
  radius_m      double precision GENERATED ALWAYS AS ((data->>'radiusKm')::double precision * 1000) STORED,
  location      geography(Point, 4326) GENERATED ALWAYS AS (
                  ST_SetSRID(ST_MakePoint((data->'location'->>'lng')::double precision, (data->'location'->>'lat')::double precision), 4326)::geography
                ) STORED,
  ts            bigint GENERATED ALWAYS AS ((data->>'createdAt')::bigint) STORED,
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX listings_location_gix ON listings USING gist (location);
CREATE INDEX listings_owner_idx ON listings (owner_id);

CREATE TABLE bookings (
  id          text PRIMARY KEY,
  data        jsonb NOT NULL,
  listing_id  text GENERATED ALWAYS AS (data->>'listingId') STORED,
  renter_id   text GENERATED ALWAYS AS (data->>'renterId') STORED,
  provider_id text GENERATED ALWAYS AS (data->>'providerId') STORED,
  status      text GENERATED ALWAYS AS (data->>'status') STORED,
  start_date  text GENERATED ALWAYS AS (data->>'startDate') STORED,
  end_date    text GENERATED ALWAYS AS (data->>'endDate') STORED,
  ts          bigint GENERATED ALWAYS AS ((data->>'createdAt')::bigint) STORED,
  updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX bookings_listing_idx ON bookings (listing_id, status);
CREATE INDEX bookings_renter_idx ON bookings (renter_id);
CREATE INDEX bookings_provider_idx ON bookings (provider_id);

-- Photo evidence: hash + server timestamp are computed by the API on upload (FR-P2/P3). Append-only (FR-P4).
CREATE TABLE photos (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  booking_id text GENERATED ALWAYS AS (data->>'bookingId') STORED,
  stage      text GENERATED ALWAYS AS (data->>'stage') STORED,
  sha256     text GENERATED ALWAYS AS (data->>'sha256') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'serverTimestamp')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX photos_booking_idx ON photos (booking_id);

CREATE TABLE messages (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  booking_id text GENERATED ALWAYS AS (data->>'bookingId') STORED,
  sender_id  text GENERATED ALWAYS AS (data->>'senderId') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'at')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX messages_booking_idx ON messages (booking_id, ts);

CREATE TABLE reviews (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  booking_id text GENERATED ALWAYS AS (data->>'bookingId') STORED,
  to_id      text GENERATED ALWAYS AS (data->>'toId') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'createdAt')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE reports (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  status     text GENERATED ALWAYS AS (data->>'status') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'createdAt')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE disputes (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  booking_id text GENERATED ALWAYS AS (data->>'bookingId') STORED,
  status     text GENERATED ALWAYS AS (data->>'status') STORED,
  layer      text GENERATED ALWAYS AS (data->>'layer') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'createdAt')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE strikes (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  user_id    text GENERATED ALWAYS AS (data->>'userId') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'at')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  actor_id   text GENERATED ALWAYS AS (data->>'actorId') STORED,
  action     text GENERATED ALWAYS AS (data->>'action') STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'at')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE wanted (
  id           text PRIMARY KEY,
  data         jsonb NOT NULL,
  requester_id text GENERATED ALWAYS AS (data->>'requesterId') STORED,
  status       text GENERATED ALWAYS AS (data->>'status') STORED,
  ts           bigint GENERATED ALWAYS AS ((data->>'createdAt')::bigint) STORED,
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notifications (
  id         text PRIMARY KEY,
  data       jsonb NOT NULL,
  user_id    text GENERATED ALWAYS AS (data->>'userId') STORED,
  read       boolean GENERATED ALWAYS AS ((data->>'read')::boolean) STORED,
  ts         bigint GENERATED ALWAYS AS ((data->>'at')::bigint) STORED,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_user_idx ON notifications (user_id, ts DESC);

-- Global settings: commission config (FR-K*), collateral rules (FR-J3), banned keywords, demo clock, counters.
CREATE TABLE settings (
  key        text PRIMARY KEY,
  value      jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- Demo sessions (DEMO_MODE): bearer token → user. Production would use OTP + refresh tokens.
CREATE TABLE sessions (
  token      text PRIMARY KEY,
  user_id    text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
