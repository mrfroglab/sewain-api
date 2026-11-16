package payments

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrfroglab/sewain-api/internal/auth"
)

func serve(h http.Handler, r *http.Request, v *auth.Viewer) *httptest.ResponseRecorder {
	if v != nil {
		r = r.WithContext(auth.WithViewer(r.Context(), *v))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHandlers(t *testing.T) {
	e := newEnv(t)
	e.booking("b1", 0)
	mux := http.NewServeMux()
	NewHandlers(e.s).Register(mux)

	post := func(body string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/v1/payments/intents", strings.NewReader(body))
	}
	if w := serve(mux, post(`{"bookingId":"b1"}`), nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("guest: %d", w.Code)
	}
	if w := serve(mux, post(`{}`), &renter); w.Code != http.StatusBadRequest {
		t.Fatalf("empty: %d", w.Code)
	}
	w := serve(mux, post(`{"bookingId":"b1","kind":"rent"}`), &renter)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var out struct{ Intent Intent }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w := serve(mux, httptest.NewRequest(http.MethodGet, "/api/v1/payments/intents/"+out.Intent.ID, nil), &other); w.Code != http.StatusNotFound {
		t.Fatalf("other user: %d", w.Code)
	}
	// Webhook: bad token 401, good token 200, unknown provider 404.
	r := e.fake.Webhook("ev1", EventPaymentSucceeded, *out.Intent.ProviderRef, out.Intent.ID, out.Intent.Amount)
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(r.Body)
	bad := httptest.NewRequest(http.MethodPost, "/api/v1/payments/webhooks/fake", bytes.NewReader(body.Bytes()))
	bad.Header.Set(FakeHeader, "x")
	if w := serve(mux, bad, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", w.Code)
	}
	good := httptest.NewRequest(http.MethodPost, "/api/v1/payments/webhooks/fake", bytes.NewReader(body.Bytes()))
	good.Header.Set(FakeHeader, "fake-token")
	if w := serve(mux, good, nil); w.Code != http.StatusOK {
		t.Fatalf("good token: %d %s", w.Code, w.Body)
	}
	unk := httptest.NewRequest(http.MethodPost, "/api/v1/payments/webhooks/paypal", bytes.NewReader(body.Bytes()))
	if w := serve(mux, unk, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown provider: %d", w.Code)
	}
	// Admin reports.
	if w := serve(mux, httptest.NewRequest(http.MethodGet, "/api/v1/admin/payments/commission?from=2026-11-01&to=2026-12-01", nil), &renter); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin report: %d", w.Code)
	}
	if w := serve(mux, httptest.NewRequest(http.MethodGet, "/api/v1/admin/payments/commission?from=x", nil), &admin); w.Code != http.StatusBadRequest {
		t.Fatalf("bad period: %d", w.Code)
	}
	for _, p := range []string{"commission", "reconcile"} {
		if w := serve(mux, httptest.NewRequest(http.MethodGet, "/api/v1/admin/payments/"+p+"?from=2026-11-01&to=2026-12-01", nil), &admin); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
	}
}
