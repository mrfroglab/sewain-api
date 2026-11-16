package payments

import (
	"context"
	"fmt"
	"time"
)

// CommissionReport sums commission and gateway fees booked in [From, To) (FR-K, PRD §14).
type CommissionReport struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Commission int64     `json:"commission"` // net commission (charged − reversed)
	Fees       int64     `json:"fees"`       // gateway fees (MDR) borne by the platform, positive number
	Net        int64     `json:"net"`        // commission − fees
	Payments   int       `json:"payments"`   // rent payments in the period
	RentValue  int64     `json:"rentValue"`  // rent value of those payments
}

// CommissionReport reads the ledger for a period.
func (s *Service) CommissionReport(ctx context.Context, from, to time.Time) (CommissionReport, error) {
	r := CommissionReport{From: from, To: to}
	err := s.db.QueryRow(ctx, `SELECT
			coalesce(sum(e.amount) FILTER (WHERE e.account = $3), 0),
			coalesce(-sum(e.amount) FILTER (WHERE e.account = $4), 0)
		FROM ledger_entries e JOIN ledger_txns t ON t.id = e.txn_id
		WHERE t.created_at >= $1 AND t.created_at < $2`, from, to, AccountCommission, AccountFees).Scan(&r.Commission, &r.Fees)
	if err != nil {
		return r, err
	}
	err = s.db.QueryRow(ctx, `SELECT count(*), coalesce(sum(rent_amount), 0) FROM payment_intents
		WHERE kind = 'rent' AND paid_at >= $1 AND paid_at < $2 AND paid_at IS NOT NULL`, from, to).Scan(&r.Payments, &r.RentValue)
	r.Net = r.Commission - r.Fees
	return r, err
}

// HypotheticalReport is "komisi seandainya aktif" (FR-K6): bookings that went ahead without a gateway rent
// payment, with the commission their agreement snapshot would have charged.
type HypotheticalReport struct {
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Bookings   int       `json:"bookings"`
	RentValue  int64     `json:"rentValue"`
	Commission int64     `json:"commission"`
}

// HypotheticalCommission reads core bookings created in [from, to) that reached ACCEPTED or later.
func (s *Service) HypotheticalCommission(ctx context.Context, from, to time.Time) (HypotheticalReport, error) {
	r := HypotheticalReport{From: from, To: to}
	err := s.db.QueryRow(ctx, `SELECT count(*), coalesce(sum(b.rent_total), 0), coalesce(sum(b.commission_amount), 0)
		FROM bookings b
		WHERE b.created_at >= $1 AND b.created_at < $2
		  AND b.status IN ('ACCEPTED', 'ACTIVE', 'RETURNED', 'COMPLETED', 'DISPUTED', 'RESOLVED')
		  AND NOT EXISTS (SELECT 1 FROM payment_intents i WHERE i.booking_id = b.id AND i.kind = 'rent' AND i.paid_at IS NOT NULL)`,
		from, to).Scan(&r.Bookings, &r.RentValue, &r.Commission)
	return r, err
}

// Discrepancy kinds found by Reconcile.
const (
	DiscStatusMismatch = "status_mismatch" // provider and our intent disagree
	DiscAmountMismatch = "amount_mismatch" // provider captured a different amount
	DiscMissingLedger  = "missing_ledger"  // paid intent without its ledger txn
	DiscNegative       = "negative_balance"
	DiscProviderError  = "provider_error"
	DiscUnbalanced     = "ledger_unbalanced"
)

// Discrepancy is one reconciliation finding.
type Discrepancy struct {
	Kind     string `json:"kind"`
	IntentID string `json:"intentId,omitempty"`
	Account  string `json:"account,omitempty"`
	Detail   string `json:"detail"`
}

// Reconcile compares the ledger with the provider for intents created in [from, to) and checks ledger
// invariants (zero total, no negative holding account).
func (s *Service) Reconcile(ctx context.Context, from, to time.Time) ([]Discrepancy, error) {
	out := []Discrepancy{}
	intents, err := queryIntents(ctx, s.db, `created_at >= $1 AND created_at < $2 AND provider_ref IS NOT NULL ORDER BY created_at`, from, to)
	if err != nil {
		return nil, err
	}
	for _, in := range intents {
		p := s.providers[in.Provider]
		if p == nil {
			out = append(out, Discrepancy{Kind: DiscProviderError, IntentID: in.ID, Detail: "provider " + in.Provider + " not configured"})
			continue
		}
		st, err := p.GetStatus(ctx, *in.ProviderRef)
		if err != nil {
			out = append(out, Discrepancy{Kind: DiscProviderError, IntentID: in.ID, Detail: err.Error()})
			continue
		}
		ours := in.Status
		if in.PaidAt != nil {
			ours = StatusPaid // refunds and late payments: the charge itself was paid
		}
		theirs := st.Status
		if theirs == StatusRefunded || theirs == StatusPartiallyRefunded {
			theirs = StatusPaid
		}
		// A pending intent the provider already expired is fine (our job catches up), and vice versa.
		bothUnpaid := ours != StatusPaid && theirs != StatusPaid
		if ours != theirs && !bothUnpaid {
			out = append(out, Discrepancy{Kind: DiscStatusMismatch, IntentID: in.ID, Detail: fmt.Sprintf("ledger %s, provider %s", ours, st.Status)})
			continue
		}
		if theirs == StatusPaid && st.Amount != in.Amount {
			out = append(out, Discrepancy{Kind: DiscAmountMismatch, IntentID: in.ID, Detail: fmt.Sprintf("intent %d, provider %d", in.Amount, st.Amount)})
		}
		if in.PaidAt != nil {
			ok, err := TxnExists(ctx, s.db, "pay:"+in.ID)
			if err != nil {
				return nil, err
			}
			if !ok {
				out = append(out, Discrepancy{Kind: DiscMissingLedger, IntentID: in.ID, Detail: "paid intent without pay:" + in.ID})
			}
		}
	}
	balances, err := AllBalances(ctx, s.db)
	if err != nil {
		return nil, err
	}
	var total int64
	for acc, b := range balances {
		total += b
		if b < 0 && !isBoundaryAccount(acc) {
			out = append(out, Discrepancy{Kind: DiscNegative, Account: acc, Detail: fmt.Sprintf("balance %d", b)})
		}
	}
	if total != 0 {
		out = append(out, Discrepancy{Kind: DiscUnbalanced, Detail: fmt.Sprintf("ledger total %d", total)})
	}
	return out, nil
}

// isBoundaryAccount: gateway accounts are negative by design, platform:fees is a cost, and
// platform:commission may dip on reversals.
func isBoundaryAccount(a string) bool {
	return a == AccountFees || a == AccountCommission || (len(a) > 8 && a[:8] == "gateway:")
}
