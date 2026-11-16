package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the subset of *pgxpool.Pool the auth and kyc packages need.
type DB interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Error is a user-facing failure. Msg is Indonesian and safe to show; Status is the HTTP status to use.
type Error struct {
	Status     int
	Msg        string
	RetryAfter time.Duration // > 0 for rate limits
	Code       string        // optional machine-readable hint, e.g. "consent_required"
}

func (e *Error) Error() string { return e.Msg }

func userErr(status int, msg string) *Error { return &Error{Status: status, Msg: msg} }

// Shared user-facing messages.
const (
	MsgLoginRequired = "Masuk dulu ya."
	MsgAdminOnly     = "Hanya admin."
	MsgBadOrigin     = "Permintaan ditolak: asal permintaan tidak dikenal."
	MsgInternal      = "Lagi ada gangguan di server. Coba lagi sebentar lagi."
)

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes {"ok":false,"error":"…"} (the v1 error shape). Non-*Error values become a generic 500
// so internal details never leak to clients.
func WriteError(w http.ResponseWriter, err error) {
	var ue *Error
	if !errors.As(err, &ue) {
		ue = userErr(http.StatusInternalServerError, MsgInternal)
	}
	body := map[string]any{"ok": false, "error": ue.Msg}
	if ue.Code != "" {
		body["code"] = ue.Code
	}
	if ue.RetryAfter > 0 {
		secs := int((ue.RetryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		body["retryAfter"] = secs
	}
	WriteJSON(w, ue.Status, body)
}
