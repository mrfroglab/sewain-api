package payments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newTestXendit(t *testing.T, h http.HandlerFunc) *Xendit {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	x, err := NewXendit(XenditConfig{BaseURL: srv.URL, SecretKey: "xnd_development_test", CallbackToken: "cb-token", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestXenditCreateQRIS(t *testing.T) {
	var got map[string]any
	x := newTestXendit(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if r.Method != http.MethodPost || r.URL.Path != "/v3/payment_requests" || !ok || user != "xnd_development_test" || pass != "" {
			t.Errorf("unexpected request %s %s auth=%v", r.Method, r.URL.Path, ok)
		}
		if r.Header.Get("api-version") != "2024-11-11" || r.Header.Get("for-user-id") != "sub-1" || r.Header.Get("with-split-rule") != "splitru_1" {
			t.Errorf("headers %v", r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(fixture(t, "xendit_create_qris_response.json"))
	})
	exp := time.Date(2026, 11, 16, 10, 30, 0, 0, time.UTC)
	ch, err := x.CreateQRIS(context.Background(), ChargeRequest{IntentID: "pi-test", Amount: 105000, ExpiresAt: exp, SubAccountID: "sub-1", SplitRuleID: "splitru_1"})
	if err != nil {
		t.Fatal(err)
	}
	if ch.ProviderRef != "pr-1a2b3c4d-0000-4000-8000-000000000001" || ch.QRString == "" || ch.Status != StatusPending {
		t.Fatalf("%+v", ch)
	}
	if got["reference_id"] != "pi-test" || got["channel_code"] != "QRIS" || got["currency"] != "IDR" || got["request_amount"] != float64(105000) ||
		got["type"] != "PAY" || got["country"] != "ID" {
		t.Fatalf("body %v", got)
	}
	if cp, _ := got["channel_properties"].(map[string]any); cp["expires_at"] != "2026-11-16T10:30:00Z" {
		t.Fatalf("channel_properties %v", got["channel_properties"])
	}
}

func TestXenditErrors(t *testing.T) {
	x := newTestXendit(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/payment_requests/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"API_VALIDATION_ERROR","message":"bad"}`))
	})
	if _, err := x.CreateQRIS(context.Background(), ChargeRequest{IntentID: "a", Amount: 1}); !errors.Is(err, ErrProviderFailed) {
		t.Fatalf("err %v", err)
	}
	if _, err := x.GetStatus(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if _, err := x.Payout(context.Background(), PayoutRequest{}); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("payout without path: %v", err)
	}
	if x.Capabilities().PartialRefund {
		t.Fatal("QRIS refunds are full only")
	}
}

func TestXenditGetStatusAndRefund(t *testing.T) {
	var refundBody map[string]any
	x := newTestXendit(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v3/payment_requests/pr-1":
			_, _ = w.Write([]byte(`{"payment_request_id":"pr-1","status":"SUCCEEDED","request_amount":105000,
				"latest_payment":{"status":"SUCCEEDED","captures":[{"capture_amount":105000}]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/refunds":
			if r.Header.Get("idempotency-key") != "rf-1" {
				t.Errorf("idempotency header %q", r.Header.Get("idempotency-key"))
			}
			_ = json.NewDecoder(r.Body).Decode(&refundBody)
			_, _ = w.Write([]byte(`{"id":"rfd-1","status":"PENDING"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	st, err := x.GetStatus(context.Background(), "pr-1")
	if err != nil || st.Status != StatusPaid || st.Amount != 105000 {
		t.Fatalf("%+v %v", st, err)
	}
	res, err := x.Refund(context.Background(), RefundRequest{RefundID: "rf-1", ProviderRef: "pr-1", Amount: 105000, Reason: "cancel"})
	if err != nil || res.ProviderRef != "rfd-1" || res.Status != "submitted" {
		t.Fatalf("%+v %v", res, err)
	}
	if refundBody["payment_request_id"] != "pr-1" || refundBody["reference_id"] != "rf-1" || refundBody["amount"] != float64(105000) {
		t.Fatalf("refund body %v", refundBody)
	}
}

func webhookReq(token string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/payments/webhooks/xendit", bytes.NewReader(body))
	if token != "" {
		r.Header.Set("x-callback-token", token)
	}
	return r
}

func TestXenditVerifyWebhook(t *testing.T) {
	x := newTestXendit(t, func(http.ResponseWriter, *http.Request) {})
	body := fixture(t, "xendit_payment_capture.json")
	ev, err := x.VerifyWebhook(webhookReq("cb-token", body), body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != EventPaymentSucceeded || ev.ProviderRef != "pr-1a2b3c4d-0000-4000-8000-000000000001" || ev.Reference != "pi-test" ||
		ev.Amount != 105000 || ev.ID != "payment.capture:cptr-0001" {
		t.Fatalf("%+v", ev)
	}
	for _, tok := range []string{"", "cb-tokem", "cb-token ", "CB-TOKEN"} {
		if _, err := x.VerifyWebhook(webhookReq(tok, body), body); !errors.Is(err, ErrBadSignature) {
			t.Errorf("token %q accepted: %v", tok, err)
		}
	}
	rb := fixture(t, "xendit_refund_succeeded.json")
	ev, err = x.VerifyWebhook(webhookReq("cb-token", rb), rb)
	if err != nil || ev.Type != EventRefundSucceeded || ev.ProviderRef != "rfd-0001" || ev.Reference != "rf-deposit-pi-test" || ev.Amount != 200000 {
		t.Fatalf("%+v %v", ev, err)
	}
	other := []byte(`{"event":"payment.authorization","created":"2026-11-16T10:00:00Z","data":{"payment_id":"py-1"}}`)
	if ev, err := x.VerifyWebhook(webhookReq("cb-token", other), other); err != nil || ev.Type != EventIgnored {
		t.Fatalf("%+v %v", ev, err)
	}
	frac := []byte(`{"event":"payment.capture","data":{"payment_id":"py-2","payment_request_id":"pr-2","request_amount":10.5}}`)
	if _, err := x.VerifyWebhook(webhookReq("cb-token", frac), frac); err == nil {
		t.Fatal("fractional IDR accepted")
	}
}

func TestNewXenditRequiresSecrets(t *testing.T) {
	if _, err := NewXendit(XenditConfig{SecretKey: "x"}); err == nil {
		t.Fatal("missing callback token accepted")
	}
}

func TestFakeWebhookToken(t *testing.T) {
	f := NewFakeProvider("tok")
	r := f.Webhook("e1", EventPaymentSucceeded, "ref", "pi", 10)
	body, _ := io.ReadAll(r.Body)
	if _, err := f.VerifyWebhook(r, body); err != nil {
		t.Fatal(err)
	}
	r.Header.Set(FakeHeader, "nope")
	if _, err := f.VerifyWebhook(r, body); !errors.Is(err, ErrBadSignature) {
		t.Fatal("wrong token accepted")
	}
	if tokenEqual("", "") {
		t.Fatal("empty configured token must never verify")
	}
}

func TestConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if c, err := ConfigFromEnv(env(nil)); err != nil || c.Mode != ModeOffline {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ConfigFromEnv(env(map[string]string{"PAYMENT_MODE": "gateway"})); err == nil {
		t.Fatal("gateway without xendit keys accepted")
	}
	c, err := ConfigFromEnv(env(map[string]string{"PAYMENT_MODE": "gateway", "XENDIT_SECRET_KEY": "k", "XENDIT_CALLBACK_TOKEN": "t"}))
	if err != nil || c.Service.MDRBasisPoints != 70 || c.Service.IntentTTL != 30*time.Minute || c.Service.CommissionActive {
		t.Fatalf("%+v %v", c, err)
	}
	if ps, err := c.Providers(); err != nil || ps[0].Name() != "xendit" {
		t.Fatalf("%v %v", ps, err)
	}
	if _, err := ConfigFromEnv(env(map[string]string{"PAYMENT_MODE": "gateway", "PAYMENT_PROVIDER": "fake", "PAYMENT_FAKE_TOKEN": "x", "APP_ENV": "production"})); err == nil {
		t.Fatal("fake provider accepted in production")
	}
	if _, err := ConfigFromEnv(env(map[string]string{"PAYMENT_MODE": "bitcoin"})); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
