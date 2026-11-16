-- Payments (ROADMAP F8.1–F8.4): payment intents, a double-entry ledger in rupiah, provider webhooks, refunds
-- and payouts. Owned by internal/payments. Only depends on users(id); booking ids are plain text so the
-- package can be installed without touching the core schema. Not used unless PAYMENT_MODE=gateway.

-- +goose Up
CREATE TABLE payment_intents (
    id                  text PRIMARY KEY,
    booking_id          text NOT NULL,
    payer_id            text NOT NULL REFERENCES users (id),
    kind                text NOT NULL CHECK (kind IN ('rent', 'deposit', 'claim')),
    amount              bigint NOT NULL CHECK (amount > 0),
    -- Breakdown of a rent intent (FR-K3/K4/K5): rent value + the renter's share of the commission. The
    -- provider's share is deducted from escrow. Copied from the agreement snapshot at creation time.
    rent_amount         bigint NOT NULL DEFAULT 0 CHECK (rent_amount >= 0),
    commission_renter   bigint NOT NULL DEFAULT 0 CHECK (commission_renter >= 0),
    commission_provider bigint NOT NULL DEFAULT 0 CHECK (commission_provider >= 0),
    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'paid', 'expired', 'failed', 'refunded', 'partially_refunded')),
    provider            text NOT NULL,
    provider_ref        text,
    qr_string           text,
    va_number           text,
    expires_at          timestamptz NOT NULL,
    paid_at             timestamptz,
    paid_late           boolean NOT NULL DEFAULT false,
    fee_amount          bigint NOT NULL DEFAULT 0 CHECK (fee_amount >= 0),
    refunded_amount     bigint NOT NULL DEFAULT 0 CHECK (refunded_amount >= 0),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CHECK (refunded_amount <= amount),
    CHECK (kind <> 'rent' OR amount = rent_amount + commission_renter)
);
-- At most one live intent per booking and kind: a retry returns the pending one, a paid one is never doubled.
-- A late payment on an expired intent while another one is paid is an overpayment and is refunded.
CREATE UNIQUE INDEX payment_intents_live_uniq ON payment_intents (booking_id, kind)
    WHERE status IN ('pending', 'paid');
CREATE UNIQUE INDEX payment_intents_provider_ref_uniq ON payment_intents (provider, provider_ref)
    WHERE provider_ref IS NOT NULL;
CREATE INDEX payment_intents_pending_idx ON payment_intents (expires_at) WHERE status = 'pending';
CREATE INDEX payment_intents_booking_idx ON payment_intents (booking_id);

-- One ledger transaction = a set of entries summing to zero, written in one DB transaction. The id is the
-- idempotency key (e.g. 'pay:<intent>', 'release:<booking>'), so replaying a step never posts twice.
CREATE TABLE ledger_txns (
    id         text PRIMARY KEY,
    kind       text NOT NULL,
    booking_id text,
    intent_id  text REFERENCES payment_intents (id),
    memo       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_txns_booking_idx ON ledger_txns (booking_id);
CREATE INDEX ledger_txns_created_idx ON ledger_txns (created_at);

-- Accounts: escrow:<booking>, deposit:<booking>, provider:<user>, renter:<user>, platform:commission,
-- platform:fees and gateway:<provider> (the money boundary: cash in/out at the gateway). Amount is signed;
-- positive increases the account's balance.
CREATE TABLE ledger_entries (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    txn_id  text NOT NULL REFERENCES ledger_txns (id),
    account text NOT NULL CHECK (account ~ '^(escrow|deposit|provider|renter|platform|gateway):.+$'),
    amount  bigint NOT NULL CHECK (amount <> 0)
);
CREATE INDEX ledger_entries_txn_idx ON ledger_entries (txn_id);
CREATE INDEX ledger_entries_account_idx ON ledger_entries (account, id);

-- +goose StatementBegin
CREATE FUNCTION ledger_txn_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    total bigint;
    n     int;
BEGIN
    SELECT coalesce(sum(amount), 0), count(*) INTO total, n FROM ledger_entries WHERE txn_id = NEW.txn_id;
    IF total <> 0 THEN
        RAISE EXCEPTION 'ledger txn % is unbalanced (sum %)', NEW.txn_id, total;
    END IF;
    IF n < 2 THEN
        RAISE EXCEPTION 'ledger txn % needs at least two entries', NEW.txn_id;
    END IF;
    RETURN NULL;
END $$;
-- +goose StatementEnd
-- Checked at commit, after every entry of the transaction is in.
CREATE CONSTRAINT TRIGGER ledger_entries_balanced AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger_txn_balanced();

-- +goose StatementBegin
CREATE FUNCTION ledger_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger is append-only; post a reversing transaction instead';
END $$;
-- +goose StatementEnd
CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();
CREATE TRIGGER ledger_txns_append_only BEFORE UPDATE OR DELETE ON ledger_txns
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();

-- Periodic balance snapshots: balance = snapshot.balance + entries with id > snapshot.last_entry_id.
CREATE TABLE ledger_balance_snapshots (
    account       text NOT NULL,
    last_entry_id bigint NOT NULL,
    balance       bigint NOT NULL,
    taken_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account, last_entry_id)
);

CREATE VIEW ledger_balances AS
WITH snap AS (
    SELECT DISTINCT ON (account) account, last_entry_id, balance
    FROM ledger_balance_snapshots ORDER BY account, last_entry_id DESC
)
SELECT a.account,
       (coalesce(s.balance, 0) + coalesce((SELECT sum(e.amount) FROM ledger_entries e
                                           WHERE e.account = a.account AND e.id > coalesce(s.last_entry_id, 0)), 0))::bigint
           AS balance
FROM (SELECT DISTINCT account FROM ledger_entries) a
LEFT JOIN snap s ON s.account = a.account;

-- Every provider callback, stored before processing. (provider, event_id) makes delivery idempotent.
CREATE TABLE webhook_events (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider     text NOT NULL,
    event_id     text NOT NULL,
    event_type   text NOT NULL,
    payload      jsonb NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    result       text NOT NULL DEFAULT '',
    UNIQUE (provider, event_id)
);

-- Money leaving the gateway back to a renter (refund of the original payment, or payout when the channel
-- cannot refund partially).
CREATE TABLE refunds (
    id           text PRIMARY KEY,
    intent_id    text NOT NULL REFERENCES payment_intents (id),
    booking_id   text NOT NULL,
    user_id      text NOT NULL REFERENCES users (id),
    amount       bigint NOT NULL CHECK (amount > 0),
    reason       text NOT NULL,
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'submitted', 'succeeded', 'failed')),
    provider_ref text,
    failure      text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refunds_status_idx ON refunds (status, created_at);
CREATE UNIQUE INDEX refunds_provider_ref_uniq ON refunds (provider_ref) WHERE provider_ref IS NOT NULL;

-- Money leaving the gateway to a user's bank/e-wallet (provider earnings, partial deposit returns).
CREATE TABLE payouts (
    id           text PRIMARY KEY,
    user_id      text NOT NULL REFERENCES users (id),
    account      text NOT NULL,
    booking_id   text,
    amount       bigint NOT NULL CHECK (amount > 0),
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'submitted', 'succeeded', 'failed')),
    provider     text NOT NULL,
    provider_ref text,
    failure      text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payouts_status_idx ON payouts (status, created_at);
CREATE UNIQUE INDEX payouts_provider_ref_uniq ON payouts (provider, provider_ref) WHERE provider_ref IS NOT NULL;

-- +goose Down
DROP TABLE payouts;
DROP TABLE refunds;
DROP TABLE webhook_events;
DROP VIEW ledger_balances;
DROP TABLE ledger_balance_snapshots;
DROP TABLE ledger_entries;
DROP TABLE ledger_txns;
DROP FUNCTION ledger_append_only();
DROP FUNCTION ledger_txn_balanced();
DROP TABLE payment_intents;
