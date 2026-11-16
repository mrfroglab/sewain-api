package payments

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/mrfroglab/sewain-api/internal/auth"
)

// MaxWebhookBytes caps a provider callback body.
const MaxWebhookBytes = 256 << 10

// Handlers exposes the payment endpoints. Not mounted yet: the core wires them with PAYMENT_MODE=gateway
// (docs/integration/payments.md). Mount on a mux wrapped by auth.Service.Middleware; the webhook route
// must be reachable without a session and without the Origin check (it authenticates by callback token).
type Handlers struct{ S *Service }

// NewHandlers returns the handlers for s.
func NewHandlers(s *Service) Handlers { return Handlers{S: s} }

// Register mounts the routes.
func (h Handlers) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/payments/intents", auth.RequireUserFunc(h.CreateIntent))
	mux.Handle("GET /api/v1/payments/intents/{id}", auth.RequireUserFunc(h.GetIntent))
	mux.HandleFunc("POST /api/v1/payments/webhooks/{provider}", h.Webhook)
	mux.Handle("GET /api/v1/admin/payments/commission", auth.RequireAdminFunc(h.CommissionReport))
	mux.Handle("GET /api/v1/admin/payments/reconcile", auth.RequireAdminFunc(h.Reconcile))
}

// CreateIntent handles POST /api/v1/payments/intents {"bookingId": "...", "kind": "rent"|"deposit"}.
func (h Handlers) CreateIntent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BookingID string `json:"bookingId"`
		Kind      string `json:"kind"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.BookingID == "" {
		auth.WriteError(w, userErr(http.StatusBadRequest, "Data pembayaran tidak lengkap."))
		return
	}
	it, err := h.S.CreateIntent(r.Context(), auth.ViewerFrom(r.Context()), in.BookingID, in.Kind)
	if err != nil && !errors.Is(err, errAlreadyPaid) {
		writeErr(w, err)
		return
	}
	if errors.Is(err, errAlreadyPaid) {
		auth.WriteJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": errAlreadyPaid.Msg, "intent": it})
		return
	}
	auth.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true, "intent": it})
}

// GetIntent handles GET /api/v1/payments/intents/{id}.
func (h Handlers) GetIntent(w http.ResponseWriter, r *http.Request) {
	it, err := h.S.GetIntent(r.Context(), auth.ViewerFrom(r.Context()), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "intent": it})
}

// Webhook handles POST /api/v1/payments/webhooks/{provider}. 2xx = stored (or duplicate); 401 = bad token;
// 5xx = processing failed and the provider should retry.
func (h Handlers) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxWebhookBytes))
	if err != nil {
		auth.WriteError(w, userErr(http.StatusRequestEntityTooLarge, "Webhook terlalu besar."))
		return
	}
	res, err := h.S.HandleWebhook(r.Context(), r.PathValue("provider"), r, body)
	if err != nil {
		writeErr(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func period(r *http.Request) (time.Time, time.Time, error) {
	from, err1 := time.Parse(time.DateOnly, r.URL.Query().Get("from"))
	to, err2 := time.Parse(time.DateOnly, r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) {
		return time.Time{}, time.Time{}, userErr(http.StatusBadRequest, "Periode tidak valid. Pakai ?from=YYYY-MM-DD&to=YYYY-MM-DD.")
	}
	jkt := time.FixedZone("WIB", 7*3600)
	return time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, jkt), time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, jkt), nil
}

// CommissionReport handles GET /api/v1/admin/payments/commission?from=&to= (dates WIB, to exclusive).
func (h Handlers) CommissionReport(w http.ResponseWriter, r *http.Request) {
	from, to, err := period(r)
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	rep, err := h.S.CommissionReport(r.Context(), from, to)
	if err != nil {
		writeErr(w, err)
		return
	}
	hyp, err := h.S.HypotheticalCommission(r.Context(), from, to)
	if err != nil {
		writeErr(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "gateway": rep, "seandainyaAktif": hyp})
}

// Reconcile handles GET /api/v1/admin/payments/reconcile?from=&to=.
func (h Handlers) Reconcile(w http.ResponseWriter, r *http.Request) {
	from, to, err := period(r)
	if err != nil {
		auth.WriteError(w, err)
		return
	}
	d, err := h.S.Reconcile(r.Context(), from, to)
	if err != nil {
		writeErr(w, err)
		return
	}
	auth.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "discrepancies": d})
}

func writeErr(w http.ResponseWriter, err error) {
	var ue *auth.Error
	if !errors.As(err, &ue) {
		log.Printf("payments: %v", err)
	}
	auth.WriteError(w, err)
}
