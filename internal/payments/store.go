package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the subset of pgx.Tx / *pgxpool.Pool used inside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB is the subset of *pgxpool.Pool the package needs.
type DB interface {
	Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// inTx runs fn in a transaction (commit on nil, rollback otherwise).
func inTx(ctx context.Context, db DB, fn func(tx pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PostTxn writes a balanced transaction. It is idempotent on t.ID: posting the same id again is a no-op
// and returns posted=false. The deferred trigger re-checks the balance at commit.
func PostTxn(ctx context.Context, q Querier, t Txn) (posted bool, err error) {
	t.Entries = Compact(t.Entries)
	if err := t.Validate(); err != nil {
		return false, err
	}
	var id string
	err = q.QueryRow(ctx, `INSERT INTO ledger_txns (id, kind, booking_id, intent_id, memo)
		VALUES ($1, $2, nullif($3, ''), nullif($4, ''), $5) ON CONFLICT (id) DO NOTHING RETURNING id`,
		t.ID, t.Kind, t.BookingID, t.IntentID, t.Memo).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	accounts := make([]string, len(t.Entries))
	amounts := make([]int64, len(t.Entries))
	for i, e := range t.Entries {
		accounts[i], amounts[i] = e.Account, e.Amount
	}
	_, err = q.Exec(ctx, `INSERT INTO ledger_entries (txn_id, account, amount)
		SELECT $1, a, m FROM unnest($2::text[], $3::bigint[]) AS u(a, m)`, t.ID, accounts, amounts)
	return err == nil, err
}

// TxnExists reports whether a ledger transaction id was posted.
func TxnExists(ctx context.Context, q Querier, id string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_txns WHERE id = $1)`, id).Scan(&ok)
	return ok, err
}

// Balance returns an account's balance (snapshot + later entries).
func Balance(ctx context.Context, q Querier, account string) (int64, error) {
	var b int64
	err := q.QueryRow(ctx, `SELECT coalesce((SELECT balance FROM ledger_balances WHERE account = $1), 0)`, account).Scan(&b)
	return b, err
}

// AllBalances returns every account's balance.
func AllBalances(ctx context.Context, q Querier) (map[string]int64, error) {
	rows, err := q.Query(ctx, `SELECT account, balance FROM ledger_balances`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var a string
		var b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out[a] = b
	}
	return out, rows.Err()
}

// SnapshotBalances stores a balance snapshot per account up to the latest entry (run periodically, e.g.
// daily; the ledger_balances view then only sums newer entries). Returns the number of accounts.
func SnapshotBalances(ctx context.Context, db DB) (int, error) {
	n := 0
	err := inTx(ctx, db, func(tx pgx.Tx) error {
		// Serialize snapshots and see a stable set of entries.
		if _, err := tx.Exec(ctx, `LOCK TABLE ledger_balance_snapshots IN EXCLUSIVE MODE`); err != nil {
			return err
		}
		var last *int64
		if err := tx.QueryRow(ctx, `SELECT max(id) FROM ledger_entries`).Scan(&last); err != nil || last == nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO ledger_balance_snapshots (account, last_entry_id, balance)
			SELECT account, $1, sum(amount) FROM ledger_entries WHERE id <= $1 GROUP BY account
			ON CONFLICT DO NOTHING`, *last)
		n = int(tag.RowsAffected())
		return err
	})
	return n, err
}

// Intent is a payment_intents row.
type Intent struct {
	ID                 string     `json:"id"`
	BookingID          string     `json:"bookingId"`
	PayerID            string     `json:"payerId"`
	Kind               string     `json:"kind"`
	Amount             int64      `json:"amount"`
	RentAmount         int64      `json:"rentAmount"`
	CommissionRenter   int64      `json:"commissionRenter"`
	CommissionProvider int64      `json:"commissionProvider"`
	Status             string     `json:"status"`
	Provider           string     `json:"provider"`
	ProviderRef        *string    `json:"providerRef,omitempty"`
	QRString           *string    `json:"qrString,omitempty"`
	VANumber           *string    `json:"vaNumber,omitempty"`
	ExpiresAt          time.Time  `json:"expiresAt"`
	PaidAt             *time.Time `json:"paidAt,omitempty"`
	PaidLate           bool       `json:"paidLate,omitempty"`
	Fee                int64      `json:"fee"`
	Refunded           int64      `json:"refunded"`
	CreatedAt          time.Time  `json:"createdAt"`
}

const intentCols = `id, booking_id, payer_id, kind, amount, rent_amount, commission_renter, commission_provider, status,
	provider, provider_ref, qr_string, va_number, expires_at, paid_at, paid_late, fee_amount, refunded_amount, created_at`

func scanIntent(row pgx.Row) (Intent, error) {
	var i Intent
	err := row.Scan(&i.ID, &i.BookingID, &i.PayerID, &i.Kind, &i.Amount, &i.RentAmount, &i.CommissionRenter,
		&i.CommissionProvider, &i.Status, &i.Provider, &i.ProviderRef, &i.QRString, &i.VANumber, &i.ExpiresAt,
		&i.PaidAt, &i.PaidLate, &i.Fee, &i.Refunded, &i.CreatedAt)
	return i, err
}

func queryIntents(ctx context.Context, q Querier, where string, args ...any) ([]Intent, error) {
	rows, err := q.Query(ctx, `SELECT `+intentCols+` FROM payment_intents WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Intent
	for rows.Next() {
		i, err := scanIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func getIntent(ctx context.Context, q Querier, where string, args ...any) (Intent, error) {
	is, err := queryIntents(ctx, q, where, args...)
	if err != nil {
		return Intent{}, err
	}
	if len(is) == 0 {
		return Intent{}, pgx.ErrNoRows
	}
	if len(is) > 1 {
		return Intent{}, fmt.Errorf("payments: %d intents match %s", len(is), where)
	}
	return is[0], nil
}
