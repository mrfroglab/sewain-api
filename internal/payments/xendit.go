package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// XenditCallbackHeader is the header Xendit puts its per-account callback token in.
// Source: https://docs.xendit.co/docs/handling-webhooks
const XenditCallbackHeader = "x-callback-token"

// XenditConfig configures the Xendit adapter. Every path and value marked "PERLU VERIFIKASI SANDBOX" in
// docs/integration/payments.md can be changed without code changes.
type XenditConfig struct {
	BaseURL       string // default https://api.xendit.co
	SecretKey     string // XENDIT_SECRET_KEY (HTTP Basic username, empty password)
	CallbackToken string // XENDIT_CALLBACK_TOKEN (dashboard → webhook verification token)

	// Payments API v3 (https://docs.xendit.co/apidocs/create-payment-request): POST /v3/payment_requests,
	// header api-version 2024-11-11.
	APIVersion         string // default 2024-11-11
	PaymentRequestPath string // default /v3/payment_requests
	// GET <PaymentRequestPath>/{id}: PERLU VERIFIKASI SANDBOX (only GET /v3/payments/{id} was confirmed).
	StatusPath string
	// QRIS channel code for Indonesia: PERLU VERIFIKASI SANDBOX (not shown in the public examples).
	QRISChannelCode string // default QRIS

	// Refunds (https://docs.xendit.co/docs/refund-payment-request): POST /refunds with reference_id,
	// payment_request_id, currency, amount, reason. QRIS: full refunds only, within 30 days.
	RefundPath string // default /refunds

	// Payouts: PERLU VERIFIKASI SANDBOX. Disabled unless PayoutPath is set explicitly.
	PayoutPath string

	HTTP *http.Client
}

// Xendit implements Provider against the Xendit Payments API v3.
type Xendit struct{ cfg XenditConfig }

// NewXendit validates the config and fills defaults.
func NewXendit(cfg XenditConfig) (*Xendit, error) {
	if cfg.SecretKey == "" || cfg.CallbackToken == "" {
		return nil, errors.New("payments: XENDIT_SECRET_KEY and XENDIT_CALLBACK_TOKEN are required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.xendit.co"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.APIVersion == "" {
		cfg.APIVersion = "2024-11-11"
	}
	if cfg.PaymentRequestPath == "" {
		cfg.PaymentRequestPath = "/v3/payment_requests"
	}
	if cfg.StatusPath == "" {
		cfg.StatusPath = cfg.PaymentRequestPath
	}
	if cfg.QRISChannelCode == "" {
		cfg.QRISChannelCode = "QRIS"
	}
	if cfg.RefundPath == "" {
		cfg.RefundPath = "/refunds"
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 20 * time.Second}
	}
	return &Xendit{cfg: cfg}, nil
}

// Name implements Provider.
func (x *Xendit) Name() string { return "xendit" }

// Capabilities implements Provider. QRIS refunds are full only, valid 30 days
// (https://docs.xendit.co/qr-codes/payment-flows/refund/).
func (x *Xendit) Capabilities() Capabilities {
	return Capabilities{PartialRefund: false, RefundWindow: 30 * 24 * time.Hour, Payout: x.cfg.PayoutPath != ""}
}

type xenditAPIError struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

func (x *Xendit) do(ctx context.Context, method, path string, headers map[string]string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, x.cfg.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(x.cfg.SecretKey, "")
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := x.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderFailed, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrProviderFailed, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var ae xenditAPIError
		_ = json.Unmarshal(raw, &ae)
		return fmt.Errorf("%w: xendit %s %s: %d %s %s", ErrProviderFailed, method, path, resp.StatusCode, ae.ErrorCode, ae.Message)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: decode xendit response: %v", ErrProviderFailed, err)
		}
	}
	return nil
}

type xenditAction struct {
	Type       string `json:"type"`
	Descriptor string `json:"descriptor"`
	Value      string `json:"value"`
}

type xenditPaymentRequest struct {
	PaymentRequestID  string          `json:"payment_request_id"`
	ReferenceID       string          `json:"reference_id"`
	Status            string          `json:"status"`
	RequestAmount     json.Number     `json:"request_amount"`
	Actions           []xenditAction  `json:"actions"`
	ChannelProperties json.RawMessage `json:"channel_properties"`
	LatestPayment     *struct {
		Status   string `json:"status"`
		Captures []struct {
			CaptureAmount json.Number `json:"capture_amount"`
		} `json:"captures"`
	} `json:"latest_payment"`
}

// xenditStatus maps payment request / payment statuses (v3) onto intent statuses.
func xenditStatus(s string) string {
	switch strings.ToUpper(s) {
	case "SUCCEEDED":
		return StatusPaid
	case "EXPIRED":
		return StatusExpired
	case "FAILED", "CANCELED":
		return StatusFailed
	default: // ACCEPTING_PAYMENTS, REQUIRES_ACTION, AUTHORIZED, PENDING
		return StatusPending
	}
}

// rupiah parses a Xendit amount; IDR amounts must be whole rupiah.
func rupiah(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, fmt.Errorf("payments: non-integer IDR amount %q", n)
	}
	return int64(f), nil
}

// CreateQRIS implements Provider (POST /v3/payment_requests, channel QRIS, dynamic QR).
func (x *Xendit) CreateQRIS(ctx context.Context, req ChargeRequest) (Charge, error) {
	body := map[string]any{
		"reference_id":   req.IntentID,
		"type":           "PAY",
		"country":        "ID",
		"currency":       "IDR",
		"request_amount": req.Amount,
		"capture_method": "AUTOMATIC",
		"channel_code":   x.cfg.QRISChannelCode,
		"channel_properties": map[string]any{
			"expires_at": req.ExpiresAt.UTC().Format(time.RFC3339),
		},
		"description": req.Description,
		"metadata":    map[string]string{"intent_id": req.IntentID},
	}
	headers := map[string]string{"api-version": x.cfg.APIVersion, "for-user-id": req.SubAccountID, "with-split-rule": req.SplitRuleID}
	var out xenditPaymentRequest
	if err := x.do(ctx, http.MethodPost, x.cfg.PaymentRequestPath, headers, body, &out); err != nil {
		return Charge{}, err
	}
	ch := Charge{ProviderRef: out.PaymentRequestID, ExpiresAt: req.ExpiresAt, Status: xenditStatus(out.Status)}
	for _, a := range out.Actions {
		if a.Descriptor == "QR_STRING" {
			ch.QRString = a.Value
		}
	}
	if ch.ProviderRef == "" || ch.QRString == "" {
		return Charge{}, fmt.Errorf("%w: xendit response without payment_request_id or QR_STRING", ErrProviderFailed)
	}
	return ch, nil
}

// GetStatus implements Provider (GET <StatusPath>/{id}, PERLU VERIFIKASI SANDBOX).
func (x *Xendit) GetStatus(ctx context.Context, ref string) (ChargeStatus, error) {
	var out xenditPaymentRequest
	err := x.do(ctx, http.MethodGet, x.cfg.StatusPath+"/"+url.PathEscape(ref), map[string]string{"api-version": x.cfg.APIVersion}, nil, &out)
	if err != nil {
		return ChargeStatus{}, err
	}
	st := ChargeStatus{ProviderRef: ref, Status: xenditStatus(out.Status)}
	if st.Status == StatusPaid {
		st.Amount, err = rupiah(out.RequestAmount)
		if out.LatestPayment != nil && len(out.LatestPayment.Captures) > 0 {
			var sum int64
			for _, c := range out.LatestPayment.Captures {
				v, e := rupiah(c.CaptureAmount)
				if e != nil {
					err = e
				}
				sum += v
			}
			st.Amount = sum
		}
	}
	return st, err
}

// Refund implements Provider (POST /refunds). Xendit settles refunds asynchronously (refund.succeeded /
// refund.failed webhooks).
func (x *Xendit) Refund(ctx context.Context, req RefundRequest) (RefundResult, error) {
	body := map[string]any{
		"reference_id":       req.RefundID,
		"payment_request_id": req.ProviderRef,
		"currency":           "IDR",
		"amount":             req.Amount,
		"reason":             "REQUESTED_BY_CUSTOMER",
		"metadata":           map[string]string{"reason": req.Reason},
	}
	var out struct {
		ID       string `json:"id"`
		RefundID string `json:"refund_id"`
		Status   string `json:"status"`
	}
	// Idempotency header: PERLU VERIFIKASI SANDBOX (harmless when ignored; reference_id is our key).
	if err := x.do(ctx, http.MethodPost, x.cfg.RefundPath, map[string]string{"idempotency-key": req.RefundID}, body, &out); err != nil {
		return RefundResult{}, err
	}
	ref := out.ID
	if ref == "" {
		ref = out.RefundID
	}
	return RefundResult{ProviderRef: ref, Status: refundStatus(out.Status)}, nil
}

func refundStatus(s string) string {
	switch strings.ToUpper(s) {
	case "SUCCEEDED":
		return "succeeded"
	case "FAILED":
		return "failed"
	}
	return "submitted"
}

// Payout implements Provider. The payout endpoint and body are PERLU VERIFIKASI SANDBOX; disabled unless
// XenditConfig.PayoutPath is set.
func (x *Xendit) Payout(ctx context.Context, req PayoutRequest) (PayoutResult, error) {
	if x.cfg.PayoutPath == "" {
		return PayoutResult{}, ErrNotSupported
	}
	body := map[string]any{
		"reference_id": req.PayoutID,
		"channel_code": req.Destination.ChannelCode,
		"channel_properties": map[string]string{
			"account_number":      req.Destination.AccountNumber,
			"account_holder_name": req.Destination.AccountName,
		},
		"amount":      req.Amount,
		"currency":    "IDR",
		"description": req.Description,
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := x.do(ctx, http.MethodPost, x.cfg.PayoutPath, map[string]string{"idempotency-key": req.PayoutID}, body, &out); err != nil {
		return PayoutResult{}, err
	}
	return PayoutResult{ProviderRef: out.ID, Status: refundStatus(out.Status)}, nil
}

// xenditWebhook is the v3 envelope (https://docs.xendit.co/apidocs/payment-webhook-notification and
// https://docs.xendit.co/apidocs/refund-webhook-notification).
type xenditWebhook struct {
	Event   string `json:"event"`
	Created string `json:"created"`
	Data    struct {
		PaymentID        string      `json:"payment_id"`
		PaymentRequestID string      `json:"payment_request_id"`
		RefundID         string      `json:"refund_id"`
		ID               string      `json:"id"` // payouts (PERLU VERIFIKASI SANDBOX)
		ReferenceID      string      `json:"reference_id"`
		Status           string      `json:"status"`
		RequestAmount    json.Number `json:"request_amount"`
		Amount           json.Number `json:"amount"`
		Captures         []struct {
			CaptureID     string      `json:"capture_id"`
			CaptureAmount json.Number `json:"capture_amount"`
		} `json:"captures"`
	} `json:"data"`
}

// VerifyWebhook implements Provider: the x-callback-token header must equal the account's token
// (constant-time), then the v3 envelope is normalised. Xendit retries up to six times and may deliver out
// of order; the event id is built from ids Xendit documents as unique (payment_id/capture_id, refund_id).
func (x *Xendit) VerifyWebhook(r *http.Request, body []byte) (Event, error) {
	if !tokenEqual(r.Header.Get(XenditCallbackHeader), x.cfg.CallbackToken) {
		return Event{}, ErrBadSignature
	}
	var w xenditWebhook
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil || w.Event == "" {
		return Event{}, fmt.Errorf("payments: malformed xendit webhook: %v", err)
	}
	ev := Event{RawType: w.Event, Reference: w.Data.ReferenceID}
	if t, err := time.Parse(time.RFC3339, w.Created); err == nil {
		ev.OccurredAt = t
	}
	var err error
	switch w.Event {
	case "payment.capture", "payment.failure":
		ev.Type = EventPaymentSucceeded
		if w.Event == "payment.failure" {
			ev.Type = EventPaymentFailed
		}
		ev.ProviderRef = w.Data.PaymentRequestID
		unique := w.Data.PaymentID
		if len(w.Data.Captures) > 0 {
			var sum int64
			for _, c := range w.Data.Captures {
				v, e := rupiah(c.CaptureAmount)
				if e != nil {
					err = e
				}
				sum += v
				unique = c.CaptureID
			}
			ev.Amount = sum
		} else {
			ev.Amount, err = rupiah(w.Data.RequestAmount)
		}
		ev.ID = w.Event + ":" + unique
		if unique == "" {
			return Event{}, errors.New("payments: xendit payment webhook without payment_id")
		}
	case "refund.succeeded", "refund.failed":
		ev.Type = EventRefundSucceeded
		if w.Event == "refund.failed" {
			ev.Type = EventRefundFailed
		}
		ev.ProviderRef = w.Data.RefundID
		ev.ID = w.Event + ":" + w.Data.RefundID
		ev.Amount, err = rupiah(w.Data.Amount)
		if w.Data.RefundID == "" {
			return Event{}, errors.New("payments: xendit refund webhook without refund_id")
		}
	case "payout.succeeded", "payout.failed": // PERLU VERIFIKASI SANDBOX
		ev.Type = EventPayoutSucceeded
		if w.Event == "payout.failed" {
			ev.Type = EventPayoutFailed
		}
		ev.ProviderRef = w.Data.ID
		ev.ID = w.Event + ":" + w.Data.ID
		ev.Amount, err = rupiah(w.Data.Amount)
		if w.Data.ID == "" {
			return Event{}, errors.New("payments: xendit payout webhook without id")
		}
	default: // payment.authorization, payment_token.* …: verified, stored, not processed
		ev.Type = EventIgnored
		ev.ID = w.Event + ":" + w.Data.PaymentID + w.Data.RefundID + w.Data.ID + ":" + w.Created
	}
	return ev, err
}
