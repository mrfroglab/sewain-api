package payments

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mrfroglab/sewain-api/internal/auth"
)

var (
	renter = auth.Viewer{UserID: "u-renter", Role: auth.RoleUser}
	other  = auth.Viewer{UserID: "u-other", Role: auth.RoleUser}
	admin  = auth.Viewer{UserID: "u-admin", Role: auth.RoleAdmin}
)

func statusOf(err error) int {
	var ue *auth.Error
	if errors.As(err, &ue) {
		return ue.Status
	}
	return 0
}

func TestFullRentalFlow(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 200000)

	if _, err := e.s.CreateIntent(e.ctx, other, "b1", KindRent); statusOf(err) != http.StatusForbidden {
		t.Fatalf("non-renter: %v", err)
	}
	rent, err := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	if err != nil {
		t.Fatal(err)
	}
	if rent.Amount != 105000 || rent.CommissionRenter != 5000 || rent.CommissionProvider != 5000 || rent.QRString == nil || rent.ProviderRef == nil {
		t.Fatalf("%+v", rent)
	}
	again, err := e.s.CreateIntent(e.ctx, renter, "b1", "")
	if err != nil || again.ID != rent.ID {
		t.Fatalf("retry should return the live intent: %v %v", again.ID, err)
	}
	dep, err := e.s.CreateIntent(e.ctx, renter, "b1", KindDeposit)
	if err != nil || dep.Amount != 200000 {
		t.Fatalf("%+v %v", dep, err)
	}
	if _, err := e.s.GetIntent(e.ctx, other, rent.ID); statusOf(err) != http.StatusNotFound {
		t.Fatalf("other user sees intent: %v", err)
	}
	if _, err := e.s.GetIntent(e.ctx, admin, rent.ID); err != nil {
		t.Fatal(err)
	}

	if r := e.pay(rent, "ev-rent"); r.Result != "paid" {
		t.Fatalf("%+v", r)
	}
	if r := e.pay(rent, "ev-rent"); !r.Duplicate {
		t.Fatalf("redelivery not detected: %+v", r)
	}
	e.pay(dep, "ev-dep")
	if _, err := e.s.CreateIntent(e.ctx, renter, "b1", KindRent); !errors.Is(err, errAlreadyPaid) {
		t.Fatalf("paid twice: %v", err)
	}
	if e.balance("escrow:b1") != 95000 || e.balance("deposit:b1") != 200000 || e.balance(AccountCommission) != 10000 ||
		e.balance(AccountFees) != -(735+1400) {
		t.Fatal("payment not booked as expected")
	}
	e.assertBalanced()

	// Handover: rent to the provider (idempotent).
	for range 2 {
		if _, err := e.s.ReleaseEscrow(e.ctx, "b1"); err != nil {
			t.Fatal(err)
		}
	}
	if e.balance("escrow:b1") != 0 || e.balance("provider:u-provider") != 95000 {
		t.Fatal("escrow not released once")
	}
	// Completed: deposit back in full via a refund of the deposit payment.
	res, err := e.s.SettleCompleted(e.ctx, "b1")
	if err != nil || res.DepositRefund != 200000 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := e.s.SettleCompleted(e.ctx, "b1"); !errors.Is(err, ErrAlreadySettled) {
		t.Fatalf("settled twice: %v", err)
	}
	if n, err := e.s.ExecuteRefunds(e.ctx, 10); err != nil || n != 1 || len(e.fake.Refunds) != 1 || e.fake.Refunds[0].Amount != 200000 {
		t.Fatalf("refunds %d %v %+v", n, err, e.fake.Refunds)
	}
	if e.balance("renter:u-renter") != 200000 {
		t.Fatal("refund owed not booked")
	}
	rf := e.fake.Refunds[0]
	e.webhook("ev-rf", EventRefundSucceeded, "fake-rf-"+rf.RefundID, rf.RefundID, 200000)
	e.webhook("ev-rf-late-fail", EventRefundFailed, "fake-rf-"+rf.RefundID, rf.RefundID, 200000) // out of order: ignored
	if e.balance("renter:u-renter") != 0 || e.intent(dep.ID).Status != StatusRefunded {
		t.Fatal("refund not settled")
	}
	// Payout of the rent to the provider.
	resolve := func(context.Context, string) (PayoutDestination, error) {
		return PayoutDestination{ChannelCode: "ID_BCA", AccountNumber: "123", AccountName: "Penyedia"}, nil
	}
	if n, err := e.s.ExecutePayouts(e.ctx, resolve, 10); err != nil || n != 1 {
		t.Fatalf("payouts %d %v", n, err)
	}
	po := e.fake.Payouts[0]
	e.webhook("ev-po", EventPayoutSucceeded, "fake-po-"+po.PayoutID, po.PayoutID, po.Amount)
	if e.balance("provider:u-provider") != 0 {
		t.Fatal("payout not booked")
	}
	e.assertBalanced()
	d, err := e.s.Reconcile(e.ctx, e.now.Add(-time.Hour), e.now.Add(time.Hour))
	if err != nil || len(d) != 0 {
		t.Fatalf("reconcile: %+v %v", d, err)
	}
}

func TestCreateIntentRules(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	if _, err := e.s.CreateIntent(e.ctx, renter, "b1", KindDeposit); !errors.Is(err, errNoDeposit) {
		t.Fatalf("deposit without cash deposit: %v", err)
	}
	if _, err := e.s.CreateIntent(e.ctx, renter, "b1", "bitcoin"); !errors.Is(err, errBadKind) {
		t.Fatal(err)
	}
	if _, err := e.s.CreateIntent(e.ctx, renter, "nope", KindRent); !errors.Is(err, errBookingNotFound) {
		t.Fatal(err)
	}
	e.exec(`UPDATE bookings SET status = 'REQUESTED' WHERE id = 'b1'`)
	if _, err := e.s.CreateIntent(e.ctx, renter, "b1", KindRent); !errors.Is(err, errNotAccepted) {
		t.Fatal(err)
	}
	e.exec(`UPDATE bookings SET status = 'ACCEPTED' WHERE id = 'b1'`)
	e.fake.FailNext = true
	if _, err := e.s.CreateIntent(e.ctx, renter, "b1", KindRent); statusOf(err) != http.StatusBadGateway {
		t.Fatalf("gateway failure: %v", err)
	}
	if in, err := e.s.CreateIntent(e.ctx, renter, "b1", KindRent); err != nil || in.QRString == nil {
		t.Fatalf("retry after gateway failure: %+v %v", in, err)
	}
}

func TestIntentExpiry(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	in, err := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	if err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(29 * time.Minute)
	if n, _ := e.s.ExpireIntents(e.ctx); n != 0 {
		t.Fatal("expired too early")
	}
	e.now = e.now.Add(time.Minute)
	if got, _ := e.s.GetIntent(e.ctx, renter, in.ID); got.Status != StatusExpired {
		t.Fatalf("status %s", got.Status)
	}
	if n, _ := e.s.ExpireIntents(e.ctx); n != 0 {
		t.Fatal("GetIntent should have stored the expiry")
	}
	fresh, err := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	if err != nil || fresh.ID == in.ID {
		t.Fatalf("new intent after expiry: %v", err)
	}
	// The renter still pays the old QR: the money is real, booked once, flagged late; the new QR retires.
	if r := e.pay(in, "ev-late"); r.Result != "paid_late" {
		t.Fatalf("%+v", r)
	}
	if got := e.intent(in.ID); got.Status != StatusPaid || !got.PaidLate {
		t.Fatalf("%+v", got)
	}
	if e.intent(fresh.ID).Status != StatusExpired {
		t.Fatal("newer pending intent not retired")
	}
	if e.balance("escrow:b1") != 95000 {
		t.Fatal("late payment not booked")
	}
	e.assertBalanced()
}

func TestOverpaymentIsRefunded(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	old, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	e.now = e.now.Add(time.Hour)
	if _, err := e.s.ExpireIntents(e.ctx); err != nil {
		t.Fatal(err)
	}
	fresh, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	e.pay(fresh, "ev-new")
	if r := e.pay(old, "ev-old"); r.Result != "overpaid: refund queued" {
		t.Fatalf("%+v", r)
	}
	if e.balance("escrow:b1") != 95000 || e.balance("renter:u-renter") != 105000 {
		t.Fatal("overpayment should sit in the renter account")
	}
	if n, _ := e.s.ExecuteRefunds(e.ctx, 5); n != 1 || e.fake.Refunds[0].ProviderRef != *old.ProviderRef {
		t.Fatalf("overpayment refund not submitted: %+v", e.fake.Refunds)
	}
	e.assertBalanced()
}

func TestWebhookOrderingAndSignature(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	in, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)

	// Wrong token: rejected, nothing stored.
	r := e.fake.Webhook("ev-x", EventPaymentSucceeded, *in.ProviderRef, in.ID, in.Amount)
	r.Header.Set(FakeHeader, "forged")
	body, _ := io.ReadAll(r.Body)
	if _, err := e.s.HandleWebhook(e.ctx, "fake", r, body); !errors.Is(err, errBadWebhook) {
		t.Fatalf("forged webhook: %v", err)
	}
	if _, err := e.s.HandleWebhook(e.ctx, "nope", r, body); !errors.Is(err, errUnknownProvider) {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM webhook_events`) != 0 {
		t.Fatal("rejected webhook stored")
	}
	// Wrong amount: stored, not booked.
	if res := e.webhook("ev-amt", EventPaymentSucceeded, *in.ProviderRef, in.ID, 1); res.Result == "paid" {
		t.Fatal("wrong amount booked")
	}
	if e.intent(in.ID).Status != StatusPending {
		t.Fatal("wrong amount changed the intent")
	}
	// Capture, then a late failure (reversed order): stays paid.
	e.pay(in, "ev-cap")
	if res := e.webhook("ev-fail", EventPaymentFailed, *in.ProviderRef, in.ID, in.Amount); res.Result != "ignored: intent paid" {
		t.Fatalf("%+v", res)
	}
	// A second capture event with another id does not book twice.
	e.pay(in, "ev-cap-2")
	if e.intent(in.ID).Status != StatusPaid || e.count(`SELECT count(*) FROM ledger_txns`) != 1 {
		t.Fatal("double booking")
	}
	// Unknown intent: error (provider retries), event not stored.
	r = e.fake.Webhook("ev-unknown", EventPaymentSucceeded, "nope", "nope", 1)
	body, _ = io.ReadAll(r.Body)
	if _, err := e.s.HandleWebhook(e.ctx, "fake", r, body); err == nil {
		t.Fatal("unknown intent accepted")
	}
	if e.count(`SELECT count(*) FROM webhook_events WHERE event_id = 'ev-unknown'`) != 0 {
		t.Fatal("failed event kept; the retry would be treated as a duplicate")
	}
	// Failure before capture (also reversed): failure first, capture later → paid late.
	e.booking("b2", 0)
	in2, _ := e.s.CreateIntent(e.ctx, renter, "b2", KindRent)
	e.webhook("ev2-fail", EventPaymentFailed, *in2.ProviderRef, in2.ID, in2.Amount)
	if res := e.pay(in2, "ev2-cap"); res.Result != "paid_late" {
		t.Fatalf("%+v", res)
	}
	e.assertBalanced()
}

func TestConcurrentWebhooks(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	in, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	var wg sync.WaitGroup
	results := make(chan WebhookResult, 20)
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "ev-same"
			if i%2 == 1 {
				id = "ev-other-capture" // a different event id for the same payment
			}
			r := e.fake.Webhook(id, EventPaymentSucceeded, *in.ProviderRef, in.ID, in.Amount)
			body, _ := io.ReadAll(r.Body)
			res, err := e.s.HandleWebhook(context.Background(), "fake", r, body)
			if err != nil {
				errs <- err
				return
			}
			results <- res
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	processed := 0
	for r := range results {
		if !r.Duplicate {
			processed++
		}
	}
	if processed != 2 {
		t.Fatalf("each event id must be processed exactly once, got %d", processed)
	}
	if e.count(`SELECT count(*) FROM ledger_txns`) != 1 || e.balance("escrow:b1") != 95000 {
		t.Fatal("payment booked more than once")
	}
	e.assertBalanced()
}

func TestConcurrentCreateIntent(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	var wg sync.WaitGroup
	ids := make(chan string, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in, err := e.s.CreateIntent(context.Background(), renter, "b1", KindRent)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- in.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 || e.count(`SELECT count(*) FROM payment_intents`) != 1 {
		t.Fatalf("intents %v", seen)
	}
}

func TestDisputeSettlementWithShortfall(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 200000)
	rent, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	dep, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindDeposit)
	e.pay(rent, "e1")
	e.pay(dep, "e2")
	if _, err := e.s.ReleaseEscrow(e.ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Settle(e.ctx, "b1", Outcome{Case: "entah"}); !errors.Is(err, ErrBadOutcome) {
		t.Fatal(err)
	}
	res, err := e.s.Settle(e.ctx, "b1", Outcome{Case: CaseRepairable, Claim: 260000})
	if err != nil {
		t.Fatal(err)
	}
	if res.DepositToProvider != 200000 || res.Shortfall != 60000 || res.ClaimIntent == nil || res.ClaimIntent.Amount != 60000 {
		t.Fatalf("%+v", res)
	}
	if e.balance("provider:u-provider") != 95000+200000 || e.count(`SELECT count(*) FROM refunds`) != 0 {
		t.Fatal("deposit not awarded")
	}
	// The renter pays the claim; it goes to the provider and a payout is queued.
	e.pay(*res.ClaimIntent, "e3")
	if e.balance("provider:u-provider") != 95000+260000 {
		t.Fatal("claim not credited")
	}
	if e.count(`SELECT count(*) FROM payouts WHERE status = 'pending'`) != 3 {
		t.Fatal("expected payouts: rent, settlement, claim")
	}
	e.assertBalanced()
}

func TestPartialDepositRefundUsesPayoutWithoutPartialRefund(t *testing.T) {
	e := newEnv(t)
	e.fake.Caps.PartialRefund = false // like QRIS at Xendit
	e.booking("b1", 200000)
	dep, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindDeposit)
	e.pay(dep, "e1")
	if _, err := e.s.Settle(e.ctx, "b1", Outcome{Case: CaseLate, Claim: 50000}); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM refunds`) != 0 ||
		e.count(`SELECT count(*) FROM payouts WHERE account = 'renter:u-renter' AND amount = 150000`) != 1 {
		t.Fatal("partial deposit return should be a payout")
	}
}

func TestCancelRefundsEverything(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 200000)
	rent, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	dep, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindDeposit)
	e.pay(rent, "e1")
	e.pay(dep, "e2")
	res, err := e.s.Settle(e.ctx, "b1", Outcome{Case: CaseNotAsDescribed})
	if err != nil || res.RentRefund+res.CommissionReversed != 105000 {
		t.Fatalf("%+v %v", res, err)
	}
	if e.balance(AccountCommission) != 0 || e.balance("renter:u-renter") != 305000 {
		t.Fatal("cancel should reverse commission and owe the renter everything")
	}
	if e.count(`SELECT count(*) FROM refunds WHERE amount IN (105000, 200000)`) != 2 {
		t.Fatal("expected two full refunds")
	}
}

func TestReconcileFindsDrift(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	e.booking("b2", 0)
	a, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	b, _ := e.s.CreateIntent(e.ctx, renter, "b2", KindRent)
	e.pay(b, "e1")
	e.fake.SetStatus(*a.ProviderRef, StatusPaid, a.Amount) // paid at the gateway, webhook lost
	e.fake.SetStatus(*b.ProviderRef, StatusPaid, b.Amount-1)
	d, err := e.s.Reconcile(e.ctx, e.now.Add(-time.Hour), e.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, x := range d {
		kinds[x.IntentID] = x.Kind
	}
	if kinds[a.ID] != DiscStatusMismatch || kinds[b.ID] != DiscAmountMismatch {
		t.Fatalf("%+v", d)
	}
}

func TestCommissionReports(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	e.booking("b2", 0) // accepted, paid outside the gateway (pilot)
	e.exec(`UPDATE bookings SET status = 'COMPLETED' WHERE id = 'b2'`)
	e.booking("b3", 0)
	e.exec(`UPDATE bookings SET status = 'DECLINED' WHERE id = 'b3'`)
	in, _ := e.s.CreateIntent(e.ctx, renter, "b1", KindRent)
	e.pay(in, "e1")
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	rep, err := e.s.CommissionReport(e.ctx, from, to)
	if err != nil || rep.Commission != 10000 || rep.Fees != 735 || rep.Net != 9265 {
		t.Fatalf("%+v %v", rep, err)
	}
	rep, _ = e.s.CommissionReport(e.ctx, e.now.Add(-time.Hour), e.now.Add(time.Hour))
	if rep.Payments != 1 || rep.RentValue != 100000 {
		t.Fatalf("%+v", rep)
	}
	hyp, err := e.s.HypotheticalCommission(e.ctx, from, to)
	if err != nil || hyp.Bookings != 1 || hyp.Commission != 10000 || hyp.RentValue != 100000 {
		t.Fatalf("seandainya aktif: %+v %v", hyp, err)
	}
}

func TestLedgerConstraints(t *testing.T) {
	e := newEnv(t)
	// Unbalanced entries are rejected at commit by the deferred trigger.
	tx, err := e.db.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tx.Exec(e.ctx, `INSERT INTO ledger_txns (id, kind) VALUES ('t1', 'x')`)
	_, _ = tx.Exec(e.ctx, `INSERT INTO ledger_entries (txn_id, account, amount) VALUES ('t1', 'escrow:b', 5), ('t1', 'provider:u', -4)`)
	if err := tx.Commit(e.ctx); err == nil {
		t.Fatal("unbalanced txn committed")
	}
	if posted, err := PostTxn(e.ctx, e.db, Txn{ID: "t2", Kind: "x", Entries: move("gateway:fake", "escrow:b", 10)}); err != nil || !posted {
		t.Fatal(err)
	}
	if posted, err := PostTxn(e.ctx, e.db, Txn{ID: "t2", Kind: "x", Entries: move("gateway:fake", "escrow:b", 10)}); err != nil || posted {
		t.Fatal("repost not idempotent")
	}
	if _, err := e.db.Exec(e.ctx, `UPDATE ledger_entries SET amount = 99`); err == nil {
		t.Fatal("ledger update allowed")
	}
	if _, err := e.db.Exec(e.ctx, `DELETE FROM ledger_txns`); err == nil {
		t.Fatal("ledger delete allowed")
	}
	// Snapshots do not change balances.
	before := e.balance("escrow:b")
	if n, err := SnapshotBalances(e.ctx, e.db); err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := PostTxn(e.ctx, e.db, Txn{ID: "t3", Kind: "x", Entries: move("gateway:fake", "escrow:b", 7)}); err != nil {
		t.Fatal(err)
	}
	if e.balance("escrow:b") != before+7 {
		t.Fatal("snapshot + later entries wrong")
	}
	e.assertBalanced()
}
