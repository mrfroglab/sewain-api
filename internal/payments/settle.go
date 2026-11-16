package payments

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
)

// lockBooking serializes money movements of one booking.
func lockBooking(ctx context.Context, tx pgx.Tx, bookingID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('payments:' || $1))`, bookingID)
	return err
}

// paidIntents returns the booking's paid intents by kind (locked).
func paidIntents(ctx context.Context, tx pgx.Tx, bookingID string) (map[string]Intent, error) {
	is, err := queryIntents(ctx, tx, `booking_id = $1 AND status = 'paid' FOR UPDATE`, bookingID)
	if err != nil {
		return nil, err
	}
	out := map[string]Intent{}
	for _, i := range is {
		out[i.Kind] = i
	}
	return out, nil
}

// FundsFor reads what the ledger holds for a booking.
func (s *Service) FundsFor(ctx context.Context, q Querier, b Booking, paid map[string]Intent) (Funds, error) {
	f := Funds{BookingID: b.ID, RenterID: b.RenterID, ProviderID: b.ProviderID}
	var err error
	if f.Escrow, err = Balance(ctx, q, EscrowAccount(b.ID)); err != nil {
		return f, err
	}
	if f.Deposit, err = Balance(ctx, q, DepositAccount(b.ID)); err != nil {
		return f, err
	}
	if r, ok := paid[KindRent]; ok {
		f.RentPaid = r.RentAmount
		f.Commission = Commission{Charged: r.CommissionRenter + r.CommissionProvider, RenterPays: r.CommissionRenter,
			ProviderPays: r.CommissionProvider}
		f.Commission.Amount = f.Commission.Charged
	}
	return f, nil
}

// ReleaseEscrow pays the rent held in escrow to the provider at handover (booking ACTIVE). Idempotent.
func (s *Service) ReleaseEscrow(ctx context.Context, bookingID string) (int64, error) {
	var released int64
	err := inTx(ctx, s.db, func(tx pgx.Tx) error {
		if err := lockBooking(ctx, tx, bookingID); err != nil {
			return err
		}
		b, err := s.bookings.Booking(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		paid, err := paidIntents(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		f, err := s.FundsFor(ctx, tx, b, paid)
		if err != nil || f.Escrow <= 0 {
			return err
		}
		posted, err := PostTxn(ctx, tx, Txn{ID: "release:" + bookingID, Kind: "release", BookingID: bookingID, Entries: PlanRelease(f)})
		if err != nil || !posted {
			return err
		}
		released = f.Escrow
		return s.queuePayout(ctx, tx, "po-rent-"+bookingID, b.ProviderID, ProviderAccount(b.ProviderID), bookingID, f.Escrow)
	})
	return released, err
}

// SettleResult is an executed settlement.
type SettleResult struct {
	Settlement
	ClaimIntent *Intent `json:"claimIntent,omitempty"`
}

// SettleCompleted refunds the deposit (and releases any rent still held) when a booking is COMPLETED
// without problems.
func (s *Service) SettleCompleted(ctx context.Context, bookingID string) (SettleResult, error) {
	return s.Settle(ctx, bookingID, Outcome{Case: CaseCompleted})
}

// Settle executes an outcome (dispute recommendation, PRD §11.3) once per booking: posts the plan, queues
// refunds to the renter and a payout to the provider, and creates a claim intent for the shortfall.
// Returns ErrAlreadySettled if the booking was settled before.
func (s *Service) Settle(ctx context.Context, bookingID string, o Outcome) (SettleResult, error) {
	var res SettleResult
	var b Booking
	err := inTx(ctx, s.db, func(tx pgx.Tx) error {
		if err := lockBooking(ctx, tx, bookingID); err != nil {
			return err
		}
		var err error
		if b, err = s.bookings.Booking(ctx, tx, bookingID); err != nil {
			return err
		}
		if done, err := TxnExists(ctx, tx, "settle:"+bookingID); err != nil || done {
			if done {
				return ErrAlreadySettled
			}
			return err
		}
		if o.Case == CaseLate && o.ClaimCap == 0 {
			o.ClaimCap = b.LateFeeCap
		}
		paid, err := paidIntents(ctx, tx, bookingID)
		if err != nil {
			return err
		}
		f, err := s.FundsFor(ctx, tx, b, paid)
		if err != nil {
			return err
		}
		plan, err := PlanSettlement(f, o)
		if err != nil {
			return err
		}
		res.Settlement = plan
		if len(plan.Entries) > 0 {
			if _, err := PostTxn(ctx, tx, Txn{ID: "settle:" + bookingID, Kind: "settle." + o.Case, BookingID: bookingID,
				Memo: fmt.Sprintf("claim=%d", o.Claim), Entries: plan.Entries}); err != nil {
				return err
			}
		}
		if err := s.queueBack(ctx, tx, b, paid[KindDeposit], plan.DepositRefund, "deposit"); err != nil {
			return err
		}
		if err := s.queueBack(ctx, tx, b, paid[KindRent], plan.RentRefund+plan.CommissionReversed, "rent"); err != nil {
			return err
		}
		if toProvider := plan.DepositToProvider + plan.RentReleased; toProvider > 0 {
			return s.queuePayout(ctx, tx, "po-settle-"+bookingID, b.ProviderID, ProviderAccount(b.ProviderID), bookingID, toProvider)
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	if res.Shortfall > 0 {
		in, err := s.createIntent(ctx, Intent{BookingID: bookingID, PayerID: b.RenterID, Kind: KindClaim, Amount: res.Shortfall})
		if err != nil {
			// The settlement is posted; the claim can be re-created with CreateClaimIntent.
			log.Printf("payments: claim intent for %s: %v", bookingID, err)
			return res, nil
		}
		res.ClaimIntent = &in
	}
	return res, nil
}

// CreateClaimIntent (re)creates the claim intent for a settled booking's shortfall.
func (s *Service) CreateClaimIntent(ctx context.Context, bookingID, renterID string, amount int64) (Intent, error) {
	return s.createIntent(ctx, Intent{BookingID: bookingID, PayerID: renterID, Kind: KindClaim, Amount: amount})
}

// queueBack returns amount to the renter: a refund of the original payment when the provider allows it
// (full amount, or partial refunds supported, within the refund window), otherwise a payout.
func (s *Service) queueBack(ctx context.Context, tx pgx.Tx, b Booking, src Intent, amount int64, what string) error {
	if amount <= 0 {
		return nil
	}
	if src.ID == "" {
		return fmt.Errorf("payments: %s refund of %d for %s without a paid intent", what, amount, b.ID)
	}
	caps := s.providers[src.Provider].Capabilities()
	inWindow := caps.RefundWindow == 0 || (src.PaidAt != nil && s.Now().Before(src.PaidAt.Add(caps.RefundWindow)))
	if inWindow && (amount == src.Amount || caps.PartialRefund) {
		return s.queueRefund(ctx, tx, src, amount, what)
	}
	return s.queuePayout(ctx, tx, "po-"+what+"-"+b.ID, b.RenterID, RenterAccount(b.RenterID), b.ID, amount)
}

func (s *Service) queueRefund(ctx context.Context, tx pgx.Tx, in Intent, amount int64, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO refunds (id, intent_id, booking_id, user_id, amount, reason)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (id) DO NOTHING`,
		"rf-"+reason+"-"+in.ID, in.ID, in.BookingID, in.PayerID, amount, reason)
	return err
}

func (s *Service) queuePayout(ctx context.Context, tx pgx.Tx, id, userID, account, bookingID string, amount int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO payouts (id, user_id, account, booking_id, amount, provider)
		VALUES ($1, $2, $3, nullif($4, ''), $5, $6) ON CONFLICT (id) DO NOTHING`,
		id, userID, account, bookingID, amount, s.cfg.Provider)
	return err
}

// ExecuteRefunds submits up to limit pending refunds to their provider (periodic job). Results arrive by
// webhook; the ledger moves only on refund.succeeded.
func (s *Service) ExecuteRefunds(ctx context.Context, limit int) (int, error) {
	n := 0
	for n < limit {
		done := false
		err := inTx(ctx, s.db, func(tx pgx.Tx) error {
			var id, provider string
			var ref *string
			var amount int64
			var reason string
			err := tx.QueryRow(ctx, `SELECT r.id, i.provider, i.provider_ref, r.amount, r.reason FROM refunds r
				JOIN payment_intents i ON i.id = r.intent_id
				WHERE r.status = 'pending' ORDER BY r.created_at FOR UPDATE OF r SKIP LOCKED LIMIT 1`).
				Scan(&id, &provider, &ref, &amount, &reason)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return nil
			}
			if err != nil {
				return err
			}
			p := s.providers[provider]
			if p == nil || ref == nil {
				_, err := tx.Exec(ctx, `UPDATE refunds SET status = 'failed', failure = 'no provider charge', updated_at = now() WHERE id = $1`, id)
				return err
			}
			out, perr := p.Refund(ctx, RefundRequest{RefundID: id, ProviderRef: *ref, Amount: amount, Reason: reason})
			if perr != nil {
				_, err := tx.Exec(ctx, `UPDATE refunds SET status = 'failed', failure = $2, updated_at = now() WHERE id = $1`, id, perr.Error())
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE refunds SET status = 'submitted', provider_ref = nullif($2, ''), updated_at = now() WHERE id = $1`, id, out.ProviderRef)
			return err
		})
		if err != nil || done {
			return n, err
		}
		n++
	}
	return n, nil
}

// DestinationResolver returns a user's payout destination (bank/e-wallet), stored by the core.
type DestinationResolver func(ctx context.Context, userID string) (PayoutDestination, error)

// ExecutePayouts submits up to limit pending payouts (periodic job). A payout without a destination stays
// pending. The ledger moves only on payout.succeeded.
func (s *Service) ExecutePayouts(ctx context.Context, resolve DestinationResolver, limit int) (int, error) {
	n := 0
	skip := []string{}
	for n < limit {
		done := false
		err := inTx(ctx, s.db, func(tx pgx.Tx) error {
			var id, userID, provider string
			var amount int64
			err := tx.QueryRow(ctx, `SELECT id, user_id, provider, amount FROM payouts
				WHERE status = 'pending' AND NOT (id = ANY($1)) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`, skip).
				Scan(&id, &userID, &provider, &amount)
			if errors.Is(err, pgx.ErrNoRows) {
				done = true
				return nil
			}
			if err != nil {
				return err
			}
			dest, err := resolve(ctx, userID)
			if err != nil {
				skip = append(skip, id)
				return nil
			}
			out, perr := s.providers[provider].Payout(ctx, PayoutRequest{PayoutID: id, Amount: amount, Destination: dest, Description: "Sewain " + id})
			if perr != nil {
				_, err := tx.Exec(ctx, `UPDATE payouts SET status = 'failed', failure = $2, updated_at = now() WHERE id = $1`, id, perr.Error())
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE payouts SET status = 'submitted', provider_ref = nullif($2, ''), updated_at = now() WHERE id = $1`, id, out.ProviderRef)
			return err
		})
		if err != nil || done {
			return n, err
		}
		n++
	}
	return n, nil
}
