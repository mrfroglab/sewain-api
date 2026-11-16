package auth

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Handlers exposes the auth HTTP endpoints. Register them on a mux that is wrapped by Service.Middleware.
type Handlers struct{ S *Service }

// Register mounts the auth routes (Go 1.22+ method patterns).
func (h Handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/otp/request", h.OTPRequest)
	mux.HandleFunc("POST /api/v1/auth/otp/verify", h.OTPVerify)
	mux.HandleFunc("POST /api/v1/auth/logout", h.Logout)
	mux.Handle("GET /api/v1/auth/sessions", RequireUserFunc(h.ListSessions))
	mux.Handle("DELETE /api/v1/auth/sessions/{id}", RequireUserFunc(h.RevokeSession))
	mux.Handle("DELETE /api/v1/auth/sessions", RequireUserFunc(h.RevokeOtherSessions))
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		WriteError(w, userErr(http.StatusBadRequest, "Format permintaan tidak valid."))
		return false
	}
	return true
}

type otpRequestBody struct {
	Phone string `json:"phone"`
}

// OTPRequest handles POST /api/v1/auth/otp/request.
func (h Handlers) OTPRequest(w http.ResponseWriter, r *http.Request) {
	var in otpRequestBody
	if !decodeJSON(w, r, &in) {
		return
	}
	res, err := h.S.RequestOTP(r.Context(), in.Phone, h.S.Meta(r))
	if err != nil {
		WriteError(w, err)
		return
	}
	out := map[string]any{
		"ok":          true,
		"phone":       res.Phone,
		"expiresAt":   res.ExpiresAt,
		"resendAfter": int(res.ResendAfter / time.Second),
		"channel":     h.S.Sender.Name(),
	}
	if res.DevCode != "" {
		out["devCode"] = res.DevCode
	}
	WriteJSON(w, http.StatusOK, out)
}

type otpVerifyBody struct {
	Phone       string `json:"phone"`
	Code        string `json:"code"`
	AcceptTerms bool   `json:"acceptTerms"`
	Transport   string `json:"transport"` // "cookie" (default) | "bearer"
}

// OTPVerify handles POST /api/v1/auth/otp/verify.
func (h Handlers) OTPVerify(w http.ResponseWriter, r *http.Request) {
	var in otpVerifyBody
	if !decodeJSON(w, r, &in) {
		return
	}
	bearer := strings.EqualFold(in.Transport, "bearer")
	if bearer && !h.S.Config.AllowBearerLogin {
		WriteError(w, userErr(http.StatusBadRequest, "Mode token tidak tersedia. Masuk lewat browser."))
		return
	}
	res, err := h.S.VerifyOTP(r.Context(), VerifyInput{Phone: in.Phone, Code: strings.TrimSpace(in.Code), AcceptTerms: in.AcceptTerms}, h.S.Meta(r))
	if err != nil {
		WriteError(w, err)
		return
	}
	out := map[string]any{
		"ok":      true,
		"userId":  res.UserID,
		"role":    res.Role,
		"isNew":   res.IsNew,
		"session": map[string]any{"id": res.Session.ID, "expiresAt": res.Session.ExpiresAt},
	}
	if bearer {
		out["token"] = res.Token
	} else {
		h.S.SetSessionCookie(w, res.Token)
	}
	WriteJSON(w, http.StatusOK, out)
}

// Logout handles POST /api/v1/auth/logout: revokes the current session and clears the cookie. Idempotent.
func (h Handlers) Logout(w http.ResponseWriter, r *http.Request) {
	v := ViewerFrom(r.Context())
	if !v.IsGuest() {
		if err := h.S.RevokeSession(r.Context(), v.UserID, v.SessionID); err != nil && err != errSessionNotFound {
			WriteError(w, err)
			return
		}
	}
	h.S.ClearSessionCookie(w)
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ListSessions handles GET /api/v1/auth/sessions.
func (h Handlers) ListSessions(w http.ResponseWriter, r *http.Request) {
	v := ViewerFrom(r.Context())
	list, err := h.S.Sessions(r.Context(), v.UserID, v.SessionID)
	if err != nil {
		WriteError(w, err)
		return
	}
	if list == nil {
		list = []Session{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "sessions": list})
}

// RevokeSession handles DELETE /api/v1/auth/sessions/{id}. Revoking the current session also clears the cookie.
func (h Handlers) RevokeSession(w http.ResponseWriter, r *http.Request) {
	v := ViewerFrom(r.Context())
	id := r.PathValue("id")
	if err := h.S.RevokeSession(r.Context(), v.UserID, id); err != nil {
		WriteError(w, err)
		return
	}
	if id == v.SessionID {
		h.S.ClearSessionCookie(w)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RevokeOtherSessions handles DELETE /api/v1/auth/sessions (all sessions except the current one).
func (h Handlers) RevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	v := ViewerFrom(r.Context())
	n, err := h.S.RevokeOtherSessions(r.Context(), v.UserID, v.SessionID)
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": n})
}
