package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// WebhookResult says what happened to a callback.
type WebhookResult struct {
	EventID   string `json:"eventId"`
	Duplicate bool   `json:"duplicate"`
	Result    string `json:"result"`
}

// HandleWebhook verifies, stores and applies one provider callback in a single transaction. Redelivery of
// the same event is a no-op (unique provider+event_id); late or out-of-order events never move money twice
// (intent rows are locked and every ledger txn id is an idempotency key). A processing error rolls the
// whole thing back so the provider retries.
func (s *Service) HandleWebhook(ctx context.Context, providerName string, r *http.Request, body []byte) (WebhookResult, error) {
	p, ok := s.providers[providerName]
	if !ok {
		return WebhookResult{}, errUnknownProvider
	}
	ev, err := p.VerifyWebhook(r, body)
	if errors.Is(err, ErrBadSignature) {
		return WebhookResult{}, errBadWebhook
	}
	if err != nil {
		return WebhookResult{}, userErr(http.StatusBadRequest, "Webhook tidak bisa dibaca.")
	}
	res := WebhookResult{EventID: ev.ID}
	payload := body
	if !json.Valid(payload) {
		payload, _ = json.Marshal(map[string]string{"raw": string(body)})
	}
	err = inTx(ctx, s.db, func(tx pgx.Tx) error {
		var rowID int64
		err := tx.QueryRow(ctx, `INSERT INTO webhook_events (provider, event_id, event_type, payload)
			VALUES ($1, $2, $3, $4) ON CONFLICT (provider, event_id) DO NOTHING RETURNING id`,
			providerName, ev.ID, ev.RawType, payload).Scan(&rowID)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Duplicate, res.Result = true, "duplicate"
			return nil
		}
		if err != nil {
			return err
		}
		res.Result, err = s.apply(ctx, tx, providerName, ev)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE webhook_events SET processed_at = $2, result = $3 WHERE id = $1`, rowID, s.Now(), res.Result)
		return err
	})
	return res, err
}

func (s *Service) apply(ctx context.Context, tx pgx.Tx, provider string, ev Event) (string, error) {
	switch ev.Type {
	case EventPaymentSucceeded, EventPaymentFailed, EventPaymentExpired:
		in, err := getIntent(ctx, tx, `provider = $1 AND (provider_ref = $2 OR id = $3) FOR UPDATE`, provider, ev.ProviderRef, ev.Reference)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("payments: webhook %s: unknown intent (ref %q, reference %q)", ev.ID, ev.ProviderRef, ev.Reference)
		}
		if err != nil {
			return "", err
		}
		switch ev.Type {
		case EventPaymentSucceeded:
			return s.markPaid(ctx, tx, in, ev)
		case EventPaymentFailed, EventPaymentExpired:
			to := StatusFailed
			if ev.Type == EventPaymentExpired {
				to = StatusExpired
			}
			if in.Status != StatusPending { // a late failure after a capture changes nothing
				return "ignored: intent " + in.Status, nil
			}
			_, err := tx.Exec(ctx, `UPDATE payment_intents SET status = $2, updated_at = now() WHERE id = $1`, in.ID, to)
			return to, err
		}
	case EventRefundSucceeded, EventRefundFailed:
		return s.applyRefund(ctx, tx, ev)
	case EventPayoutSucceeded, EventPayoutFailed:
		return s.applyPayout(ctx, tx, provider, ev)
	}
	return "ignored", nil
}

// markPaid books a captured payment. pending → paid; expired/failed → paid_late (the money is real). If
// another intent of the same kind is already paid, this one is an overpayment and is refunded in full.
func (s *Service) markPaid(ctx context.Context, tx pgx.Tx, in Intent, ev Event) (string, error) {
	if in.PaidAt != nil {
		return "ignored: already paid", nil
	}
	if ev.Amount != in.Amount {
		// Never book an amount we did not ask for; reconciliation reports it for a human.
		return fmt.Sprintf("amount_mismatch: got %d want %d", ev.Amount, in.Amount), nil
	}
	now := s.Now()
	late := in.Status != StatusPending
	fee := MDR(in.Amount, s.cfg.MDRBasisPoints)
	b, err := s.bookings.Booking(ctx, tx, in.BookingID)
	if err != nil {
		return "", err
	}
	// Retire a newer pending intent for the same booking+kind; detect an already paid one.
	others, err := queryIntents(ctx, tx, `booking_id = $1 AND kind = $2 AND id <> $3 AND status IN ('pending', 'paid') FOR UPDATE`,
		in.BookingID, in.Kind, in.ID)
	if err != nil {
		return "", err
	}
	overpaid := false
	for _, o := range others {
		if o.Status == StatusPaid {
			overpaid = true
		} else if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status = 'expired', updated_at = $2 WHERE id = $1`, o.ID, now); err != nil {
			return "", err
		}
	}
	status := StatusPaid
	if overpaid {
		status = in.Status // stays expired/failed; the refund below moves it to refunded
	}
	if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status = $2, paid_at = $3, paid_late = $4, fee_amount = $5,
			updated_at = $3 WHERE id = $1`, in.ID, status, now, late, fee); err != nil {
		return "", err
	}
	gw := GatewayAccount(in.Provider)
	var entries []Entry
	if overpaid {
		entries = Compact(append(move(gw, RenterAccount(in.PayerID), in.Amount), move(AccountFees, gw, fee)...))
	} else {
		entries, err = PlanPayment(PaidIntent{Kind: in.Kind, Amount: in.Amount, BookingID: in.BookingID, ProviderID: b.ProviderID,
			CommissionRenter: in.CommissionRenter, CommissionProvider: in.CommissionProvider, Provider: in.Provider, Fee: fee})
		if err != nil {
			return "", err
		}
	}
	if _, err := PostTxn(ctx, tx, Txn{ID: "pay:" + in.ID, Kind: "payment." + in.Kind, BookingID: in.BookingID,
		IntentID: in.ID, Memo: ev.ID, Entries: entries}); err != nil {
		return "", err
	}
	switch {
	case overpaid:
		if err := s.queueRefund(ctx, tx, in, in.Amount, "overpayment"); err != nil {
			return "", err
		}
		return "overpaid: refund queued", nil
	case in.Kind == KindClaim:
		// Claim money goes straight to the provider.
		if err := s.queuePayout(ctx, tx, "po-claim-"+in.ID, b.ProviderID, ProviderAccount(b.ProviderID), in.BookingID, in.Amount); err != nil {
			return "", err
		}
	}
	if late {
		return "paid_late", nil
	}
	return "paid", nil
}

func (s *Service) applyRefund(ctx context.Context, tx pgx.Tx, ev Event) (string, error) {
	var id, intentID, userID, bookingID, status string
	var amount int64
	err := tx.QueryRow(ctx, `SELECT id, intent_id, user_id, booking_id, amount, status FROM refunds
		WHERE provider_ref = $1 OR id = $2 FOR UPDATE`, ev.ProviderRef, ev.Reference).
		Scan(&id, &intentID, &userID, &bookingID, &amount, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("payments: webhook %s: unknown refund", ev.ID)
	}
	if err != nil {
		return "", err
	}
	if status == "succeeded" {
		return "ignored: refund already succeeded", nil
	}
	if ev.Type == EventRefundFailed {
		_, err := tx.Exec(ctx, `UPDATE refunds SET status = 'failed', failure = $2, updated_at = now() WHERE id = $1`, id, ev.RawType)
		return "refund_failed", err
	}
	var provider string
	if err := tx.QueryRow(ctx, `UPDATE payment_intents SET refunded_amount = refunded_amount + $2,
			status = CASE WHEN refunded_amount + $2 >= amount THEN 'refunded' ELSE 'partially_refunded' END, updated_at = now()
		WHERE id = $1 RETURNING provider`, intentID, amount).Scan(&provider); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE refunds SET status = 'succeeded', provider_ref = coalesce(provider_ref, nullif($2, '')),
			updated_at = now() WHERE id = $1`, id, ev.ProviderRef); err != nil {
		return "", err
	}
	_, err = PostTxn(ctx, tx, Txn{ID: "refund:" + id, Kind: "refund", BookingID: bookingID, IntentID: intentID, Memo: ev.ID,
		Entries: move(RenterAccount(userID), GatewayAccount(provider), amount)})
	return "refund_succeeded", err
}

func (s *Service) applyPayout(ctx context.Context, tx pgx.Tx, provider string, ev Event) (string, error) {
	var id, account, status string
	var bookingID *string
	var amount int64
	err := tx.QueryRow(ctx, `SELECT id, account, booking_id, amount, status FROM payouts
		WHERE provider = $1 AND (provider_ref = $2 OR id = $3) FOR UPDATE`, provider, ev.ProviderRef, ev.Reference).
		Scan(&id, &account, &bookingID, &amount, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("payments: webhook %s: unknown payout", ev.ID)
	}
	if err != nil {
		return "", err
	}
	if status == "succeeded" {
		return "ignored: payout already succeeded", nil
	}
	if ev.Type == EventPayoutFailed {
		_, err := tx.Exec(ctx, `UPDATE payouts SET status = 'failed', failure = $2, updated_at = now() WHERE id = $1`, id, ev.RawType)
		return "payout_failed", err
	}
	if _, err := tx.Exec(ctx, `UPDATE payouts SET status = 'succeeded', updated_at = now() WHERE id = $1`, id); err != nil {
		return "", err
	}
	b := ""
	if bookingID != nil {
		b = *bookingID
	}
	_, err = PostTxn(ctx, tx, Txn{ID: "payout:" + id, Kind: "payout", BookingID: b, Memo: ev.ID,
		Entries: move(account, GatewayAccount(provider), amount)})
	return "payout_succeeded", err
}
