package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// FakeHeader carries the fake provider's callback token.
const FakeHeader = "X-Fake-Callback-Token"

// FakeProvider is an in-memory gateway for tests and a local sandbox demo. Charges are "paid" by building
// a webhook with Webhook/PaymentWebhook and posting it to the webhook endpoint (or calling the service).
type FakeProvider struct {
	Token string // callback token checked by VerifyWebhook
	Caps  Capabilities

	mu      sync.Mutex
	seq     int
	charges map[string]*fakeCharge
	// Refunds and Payouts record the requests the provider received.
	Refunds []RefundRequest
	Payouts []PayoutRequest
	// FailNext makes the next API call fail with ErrProviderFailed.
	FailNext bool
}

type fakeCharge struct {
	req    ChargeRequest
	status string
	paid   int64
}

type fakeWebhook struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Ref       string    `json:"ref"`
	Reference string    `json:"reference"`
	Amount    int64     `json:"amount"`
	At        time.Time `json:"at"`
}

// NewFakeProvider returns a fake gateway with full capabilities.
func NewFakeProvider(token string) *FakeProvider {
	return &FakeProvider{Token: token, Caps: Capabilities{PartialRefund: true, Payout: true}, charges: map[string]*fakeCharge{}}
}

// Name implements Provider.
func (f *FakeProvider) Name() string { return "fake" }

// Capabilities implements Provider.
func (f *FakeProvider) Capabilities() Capabilities { return f.Caps }

func (f *FakeProvider) fail() error {
	if f.FailNext {
		f.FailNext = false
		return ErrProviderFailed
	}
	return nil
}

// CreateQRIS implements Provider.
func (f *FakeProvider) CreateQRIS(_ context.Context, req ChargeRequest) (Charge, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return Charge{}, err
	}
	f.seq++
	ref := fmt.Sprintf("fake-pr-%d", f.seq)
	f.charges[ref] = &fakeCharge{req: req, status: StatusPending}
	return Charge{ProviderRef: ref, QRString: "00020101021226FAKEQRIS" + req.IntentID, ExpiresAt: req.ExpiresAt, Status: StatusPending}, nil
}

// GetStatus implements Provider.
func (f *FakeProvider) GetStatus(_ context.Context, ref string) (ChargeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return ChargeStatus{}, err
	}
	c, ok := f.charges[ref]
	if !ok {
		return ChargeStatus{}, ErrNotFound
	}
	return ChargeStatus{ProviderRef: ref, Status: c.status, Amount: c.paid}, nil
}

// SetStatus changes a charge's status at the "provider" (to simulate drift for reconciliation tests).
func (f *FakeProvider) SetStatus(ref, status string, paid int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.charges[ref]; ok {
		c.status, c.paid = status, paid
	}
}

// Refund implements Provider.
func (f *FakeProvider) Refund(_ context.Context, req RefundRequest) (RefundResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return RefundResult{}, err
	}
	c, ok := f.charges[req.ProviderRef]
	if !ok {
		return RefundResult{}, ErrNotFound
	}
	if !f.Caps.PartialRefund && req.Amount != c.paid {
		return RefundResult{}, ErrNotSupported
	}
	f.Refunds = append(f.Refunds, req)
	return RefundResult{ProviderRef: "fake-rf-" + req.RefundID, Status: "submitted"}, nil
}

// Payout implements Provider.
func (f *FakeProvider) Payout(_ context.Context, req PayoutRequest) (PayoutResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(); err != nil {
		return PayoutResult{}, err
	}
	if !f.Caps.Payout {
		return PayoutResult{}, ErrNotSupported
	}
	f.Payouts = append(f.Payouts, req)
	return PayoutResult{ProviderRef: "fake-po-" + req.PayoutID, Status: "submitted"}, nil
}

// VerifyWebhook implements Provider.
func (f *FakeProvider) VerifyWebhook(r *http.Request, body []byte) (Event, error) {
	if !tokenEqual(r.Header.Get(FakeHeader), f.Token) {
		return Event{}, ErrBadSignature
	}
	var w fakeWebhook
	if err := json.Unmarshal(body, &w); err != nil || w.ID == "" || w.Type == "" {
		return Event{}, fmt.Errorf("%w: malformed fake webhook", ErrBadSignature)
	}
	return Event{ID: w.ID, Type: w.Type, RawType: w.Type, ProviderRef: w.Ref, Reference: w.Reference, Amount: w.Amount, OccurredAt: w.At}, nil
}

// Webhook builds a signed callback request. eventID makes it unique; reuse it to simulate a redelivery.
// For payment events it also updates the fake charge so GetStatus agrees.
func (f *FakeProvider) Webhook(eventID, eventType, ref, reference string, amount int64) *http.Request {
	f.mu.Lock()
	if c, ok := f.charges[ref]; ok {
		switch eventType {
		case EventPaymentSucceeded:
			c.status, c.paid = StatusPaid, amount
		case EventPaymentFailed:
			c.status = StatusFailed
		case EventPaymentExpired:
			c.status = StatusExpired
		}
	}
	f.mu.Unlock()
	body, _ := json.Marshal(fakeWebhook{ID: eventID, Type: eventType, Ref: ref, Reference: reference, Amount: amount, At: time.Now().UTC()})
	r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/payments/webhooks/fake", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(FakeHeader, f.Token)
	return r
}

// Charge returns the request a fake charge was created with.
func (f *FakeProvider) Charge(ref string) (ChargeRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.charges[ref]
	if !ok {
		return ChargeRequest{}, false
	}
	return c.req, true
}
