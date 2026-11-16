package payments

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"time"
)

// Intent kinds and statuses (payment_intents).
const (
	KindRent    = "rent"
	KindDeposit = "deposit"
	KindClaim   = "claim" // shortfall after a dispute (claim above the deposit)

	StatusPending           = "pending"
	StatusPaid              = "paid"
	StatusExpired           = "expired"
	StatusFailed            = "failed"
	StatusRefunded          = "refunded"
	StatusPartiallyRefunded = "partially_refunded"
)

// Normalised webhook event types. Providers map their own events onto these.
const (
	EventPaymentSucceeded = "payment.succeeded"
	EventPaymentFailed    = "payment.failed"
	EventPaymentExpired   = "payment.expired"
	EventRefundSucceeded  = "refund.succeeded"
	EventRefundFailed     = "refund.failed"
	EventPayoutSucceeded  = "payout.succeeded"
	EventPayoutFailed     = "payout.failed"
	EventIgnored          = "ignored" // verified but irrelevant (stored, not processed)
)

// Provider errors.
var (
	ErrBadSignature   = errors.New("payments: webhook signature invalid")
	ErrNotSupported   = errors.New("payments: operation not supported by provider")
	ErrNotFound       = errors.New("payments: not found at provider")
	ErrProviderFailed = errors.New("payments: provider request failed")
)

// ChargeRequest asks for a QRIS (or VA) charge for one intent.
type ChargeRequest struct {
	IntentID    string // our id, sent as the provider's reference id
	Amount      int64
	ExpiresAt   time.Time
	Description string
	// xenPlatform (design, needs a business account): the sub-account that owns the transaction and the
	// split rule to apply. Empty = master account, no split.
	SubAccountID string
	SplitRuleID  string
}

// Charge is a created charge.
type Charge struct {
	ProviderRef string
	QRString    string
	VANumber    string
	ExpiresAt   time.Time
	Status      string // our intent status
}

// ChargeStatus is a provider's view of a charge, for reconciliation.
type ChargeStatus struct {
	ProviderRef string
	Status      string // our intent status (pending, paid, expired, failed, refunded …)
	Amount      int64  // amount paid (0 when unpaid)
}

// RefundRequest refunds (part of) a paid charge back to its original payment method.
type RefundRequest struct {
	RefundID    string // our id (reference)
	ProviderRef string // the charge being refunded
	Amount      int64
	Reason      string
}

// RefundResult is the provider's answer; refunds settle asynchronously through webhooks.
type RefundResult struct {
	ProviderRef string
	Status      string // submitted | succeeded | failed
}

// PayoutRequest sends money to a user's bank account or e-wallet.
type PayoutRequest struct {
	PayoutID    string
	Amount      int64
	Destination PayoutDestination
	Description string
}

// PayoutDestination is a bank account or e-wallet (channel codes are the provider's).
type PayoutDestination struct {
	ChannelCode   string `json:"channelCode"`
	AccountNumber string `json:"accountNumber"`
	AccountName   string `json:"accountName"`
}

// PayoutResult is the provider's answer; payouts settle asynchronously through webhooks.
type PayoutResult struct {
	ProviderRef string
	Status      string // submitted | succeeded | failed
}

// Event is a verified, normalized webhook.
type Event struct {
	ID          string // unique per provider (idempotency key)
	Type        string // Event* constant
	RawType     string // provider's own event name
	ProviderRef string // charge/refund/payout id at the provider
	Reference   string // our id (intent, refund or payout) when the provider echoes it
	Amount      int64
	OccurredAt  time.Time
}

// Capabilities describe what a provider can do.
type Capabilities struct {
	PartialRefund bool // QRIS at Xendit: no (full refunds only, within 30 days)
	RefundWindow  time.Duration
	Payout        bool
}

// Provider is a payment gateway (ROADMAP F8.2).
type Provider interface {
	Name() string
	Capabilities() Capabilities
	CreateQRIS(ctx context.Context, req ChargeRequest) (Charge, error)
	GetStatus(ctx context.Context, providerRef string) (ChargeStatus, error)
	Refund(ctx context.Context, req RefundRequest) (RefundResult, error)
	Payout(ctx context.Context, req PayoutRequest) (PayoutResult, error)
	// VerifyWebhook authenticates a callback and normalizes it. body is the raw request body.
	VerifyWebhook(r *http.Request, body []byte) (Event, error)
}

// VAProvider is optionally implemented by providers that issue virtual accounts.
type VAProvider interface {
	CreateVA(ctx context.Context, req ChargeRequest, bankCode string) (Charge, error)
}

// SplitRoute is one destination of a split rule (xenPlatform).
type SplitRoute struct {
	ReferenceID          string
	DestinationAccountID string // sub-account business id; empty = platform (master) account
	FlatAmount           int64  // either flat …
	PercentBasisPoints   int64  // … or percent in basis points (10% = 1000)
}

// Splitter is the xenPlatform design (split rules + transfers between accounts). Implemented once Sewain
// has a business account; see docs/integration/payments.md §6.
type Splitter interface {
	CreateSplitRule(ctx context.Context, name string, routes []SplitRoute) (ruleID string, err error)
	Transfer(ctx context.Context, reference, fromAccountID, toAccountID string, amount int64) error
}

// tokenEqual compares a callback token in constant time.
func tokenEqual(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
