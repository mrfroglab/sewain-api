-- Notifications beyond in-app (ROADMAP F4.3–F4.6): push subscriptions, per-user preferences and the outbox.
-- Owned by the notify package. Depends only on users(id).

-- +goose Up
CREATE TABLE push_subscriptions (
    id              text PRIMARY KEY DEFAULT ('ps-' || replace(gen_random_uuid()::text, '-', '')),
    user_id         text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    endpoint        text NOT NULL UNIQUE,
    p256dh          text NOT NULL,
    auth            text NOT NULL,
    user_agent      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    last_success_at timestamptz,
    CHECK (endpoint LIKE 'https://%' OR endpoint LIKE 'http://%')
);
CREATE INDEX push_subscriptions_user_idx ON push_subscriptions (user_id);

-- One row per user that changed something; no row = defaults. In-app is always on and not stored.
CREATE TABLE notification_prefs (
    user_id          text PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    webpush_booking  boolean NOT NULL DEFAULT true,
    webpush_chat     boolean NOT NULL DEFAULT true,
    webpush_sengketa boolean NOT NULL DEFAULT true,
    webpush_sistem   boolean NOT NULL DEFAULT true,
    email_booking    boolean NOT NULL DEFAULT false,
    email_chat       boolean NOT NULL DEFAULT false,
    email_sengketa   boolean NOT NULL DEFAULT true,
    email_sistem     boolean NOT NULL DEFAULT true,
    whatsapp_booking  boolean NOT NULL DEFAULT false,
    whatsapp_chat     boolean NOT NULL DEFAULT false,
    whatsapp_sengketa boolean NOT NULL DEFAULT false,
    whatsapp_sistem   boolean NOT NULL DEFAULT false,
    quiet_enabled    boolean NOT NULL DEFAULT true,
    quiet_start      time NOT NULL DEFAULT '22:00',
    quiet_end        time NOT NULL DEFAULT '06:00',
    timezone         text NOT NULL DEFAULT 'Asia/Jakarta',
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE notification_outbox (
    id              text PRIMARY KEY DEFAULT ('nob-' || replace(gen_random_uuid()::text, '-', '')),
    user_id         text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    notification_id text NOT NULL, -- in-app notification id owned by the core
    channel         text NOT NULL CHECK (channel IN ('webpush', 'email', 'whatsapp')),
    payload         jsonb NOT NULL,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sent', 'failed', 'dead')),
    attempts        integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    UNIQUE (notification_id, channel)
);
-- Dispatcher claim query: due rows that still need work.
CREATE INDEX notification_outbox_due_idx ON notification_outbox (next_attempt_at)
    WHERE status IN ('pending', 'failed');
CREATE INDEX notification_outbox_status_idx ON notification_outbox (status, next_attempt_at);
CREATE INDEX notification_outbox_user_idx ON notification_outbox (user_id, created_at DESC);

-- +goose Down
DROP TABLE notification_outbox;
DROP TABLE notification_prefs;
DROP TABLE push_subscriptions;
