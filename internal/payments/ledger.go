// Package payments holds money for Sewain bookings through a licensed payment gateway (ROADMAP Phase 8,
// PRD §9.3, FR-J8, FR-K1–K8, §11.3). Every rupiah is an int64 in a double-entry ledger; nothing here uses
// floating point for money. The package is only active with PAYMENT_MODE=gateway and is wired by the core
// service (see docs/integration/payments.md).
package payments

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Account names. Per-booking and per-user accounts carry the id after the colon.
const (
	AccountCommission = "platform:commission" // commission earned (FR-K4)
	AccountFees       = "platform:fees"       // gateway fees (MDR) borne by the platform; negative = cost
)

// EscrowAccount holds the rent of one booking until handover.
func EscrowAccount(bookingID string) string { return "escrow:" + bookingID }

// DepositAccount holds a booking's cash deposit while the gateway keeps it (FR-J8).
func DepositAccount(bookingID string) string { return "deposit:" + bookingID }

// ProviderAccount is what Sewain owes a provider (paid out later).
func ProviderAccount(userID string) string { return "provider:" + userID }

// RenterAccount is what Sewain owes a renter (refunds not yet executed).
func RenterAccount(userID string) string { return "renter:" + userID }

// GatewayAccount is the money boundary at a gateway: payments in make it negative, refunds/payouts out
// bring it back. Minus its balance = cash Sewain's merchant balance should hold for that provider.
func GatewayAccount(provider string) string { return "gateway:" + provider }

// Entry moves Amount (signed rupiah, positive = the account's balance grows) on one account.
type Entry struct {
	Account string `json:"account"`
	Amount  int64  `json:"amount"`
}

// Txn is one ledger transaction. ID is its idempotency key.
type Txn struct {
	ID        string
	Kind      string
	BookingID string
	IntentID  string
	Memo      string
	Entries   []Entry
}

// ErrUnbalanced is returned for a transaction whose entries do not sum to zero.
var ErrUnbalanced = errors.New("payments: unbalanced ledger transaction")

// Validate checks that a transaction is postable: an id, at least two non-zero entries on known account
// kinds, and a zero sum.
func (t Txn) Validate() error {
	if t.ID == "" {
		return errors.New("payments: ledger txn without id")
	}
	if len(t.Entries) < 2 {
		return fmt.Errorf("payments: ledger txn %s needs at least two entries", t.ID)
	}
	var sum int64
	for _, e := range t.Entries {
		if e.Amount == 0 {
			return fmt.Errorf("payments: zero entry on %s in txn %s", e.Account, t.ID)
		}
		if !validAccount(e.Account) {
			return fmt.Errorf("payments: unknown account %q in txn %s", e.Account, t.ID)
		}
		sum += e.Amount
	}
	if sum != 0 {
		return fmt.Errorf("%w: %s sums to %d", ErrUnbalanced, t.ID, sum)
	}
	return nil
}

func validAccount(a string) bool {
	kind, id, ok := strings.Cut(a, ":")
	if !ok || id == "" {
		return false
	}
	switch kind {
	case "escrow", "deposit", "provider", "renter", "platform", "gateway":
		return true
	}
	return false
}

// Sum returns the total of entries (zero for a balanced set).
func Sum(entries []Entry) int64 {
	var s int64
	for _, e := range entries {
		s += e.Amount
	}
	return s
}

// Compact merges entries per account, drops zeros and sorts by account, so plans are deterministic.
func Compact(entries []Entry) []Entry {
	m := map[string]int64{}
	for _, e := range entries {
		m[e.Account] += e.Amount
	}
	out := make([]Entry, 0, len(m))
	for a, v := range m {
		if v != 0 {
			out = append(out, Entry{Account: a, Amount: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

// move returns the two entries that transfer amount from one account to another.
func move(from, to string, amount int64) []Entry {
	if amount == 0 {
		return nil
	}
	return []Entry{{Account: from, Amount: -amount}, {Account: to, Amount: amount}}
}

// Balances applies entries to a balance map (used by tests and in-memory reasoning).
func Balances(txns ...[]Entry) map[string]int64 {
	b := map[string]int64{}
	for _, es := range txns {
		for _, e := range es {
			b[e.Account] += e.Amount
		}
	}
	return b
}
