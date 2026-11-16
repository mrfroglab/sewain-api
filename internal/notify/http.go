package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// DefaultPushHosts are the push services browsers use. Subscription endpoints must be https on one of these
// hosts (or a subdomain), so the server never POSTs to arbitrary URLs a user registers.
var DefaultPushHosts = []string{
	"fcm.googleapis.com",        // Chrome, Edge (Android), Opera, Samsung
	"android.googleapis.com",    // legacy Chrome endpoints
	"push.services.mozilla.com", // Firefox (updates.push.services.mozilla.com)
	"notify.windows.com",        // Edge on Windows (*.notify.windows.com)
	"push.apple.com",            // Safari (web.push.apple.com)
}

// MaxSubscriptionsPerUser bounds stored subscriptions; the oldest are removed beyond it.
const MaxSubscriptionsPerUser = 10

// API serves the push and preference endpoints.
type API struct {
	DB DB
	// Viewer returns the signed-in user id, or "" for guests.
	Viewer func(r *http.Request) string
	// VAPIDPublicKey is served to browsers for PushManager.subscribe. Empty disables push endpoints (404).
	VAPIDPublicKey string
	// Available reports configured channels (Config.Channels); shown to the web so it can hide dead options.
	Available map[Channel]bool
	// PushHosts overrides DefaultPushHosts. "*" allows any https host (tests only).
	PushHosts []string
}

// Register mounts the endpoints on mux (Go 1.22+ patterns).
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/push/vapid-key", a.vapidKey)
	mux.HandleFunc("POST /api/v1/push/subscriptions", a.subscribe)
	mux.HandleFunc("DELETE /api/v1/push/subscriptions", a.unsubscribe)
	mux.HandleFunc("GET /api/v1/me/notification-prefs", a.getPrefs)
	mux.HandleFunc("PUT /api/v1/me/notification-prefs", a.putPrefs)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

func (a *API) viewer(w http.ResponseWriter, r *http.Request) (string, bool) {
	var id string
	if a.Viewer != nil {
		id = a.Viewer(r)
	}
	if id == "" {
		writeErr(w, http.StatusUnauthorized, "Masuk dulu, ya.")
		return "", false
	}
	return id, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "Data yang dikirim tidak bisa dibaca.")
		return false
	}
	return true
}

func (a *API) vapidKey(w http.ResponseWriter, r *http.Request) {
	if a.VAPIDPublicKey == "" {
		writeErr(w, http.StatusNotFound, "Notifikasi push belum aktif di server ini.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "publicKey": a.VAPIDPublicKey})
}

// SubscriptionInput is PushSubscription.toJSON() from the browser.
type SubscriptionInput struct {
	Endpoint       string `json:"endpoint"`
	ExpirationTime *int64 `json:"expirationTime"`
	Keys           struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func (a *API) allowedEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(raw) > 1024 {
		return false
	}
	hosts := a.PushHosts
	if hosts == nil {
		hosts = DefaultPushHosts
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range hosts {
		if h == "*" || host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// Validate checks the endpoint and the key sizes (P-256 point, 16-byte auth secret).
func (a *API) validate(in SubscriptionInput) error {
	if !a.allowedEndpoint(in.Endpoint) {
		return UserError("Layanan push dari browser ini tidak dikenal.")
	}
	if b, err := decodeKey(in.Keys.P256dh); err != nil || len(b) != 65 || b[0] != 4 {
		return UserError("Kunci langganan push tidak valid.")
	}
	if b, err := decodeKey(in.Keys.Auth); err != nil || len(b) != 16 {
		return UserError("Kunci langganan push tidak valid.")
	}
	return nil
}

// SaveSubscription stores (or moves to userID) a browser subscription and trims old ones beyond the limit.
// ErrSubscriptionTaken: the endpoint belongs to another account with different keys.
var ErrSubscriptionTaken = UserError("Langganan push ini terdaftar di akun lain. Matikan lalu aktifkan lagi notifikasi di browser ini.")

func SaveSubscription(ctx context.Context, db DB, userID string, in SubscriptionInput, userAgent string) error {
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	// An endpoint registered by another account only moves when the keys match too (same browser, other account
	// signed in). Knowing someone's endpoint URL alone must not let an attacker take over or break their push
	// (review L2).
	tag, err := db.Exec(ctx, `
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, user_agent)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (endpoint) DO UPDATE SET user_id = excluded.user_id, p256dh = excluded.p256dh,
			auth = excluded.auth, user_agent = excluded.user_agent, updated_at = now()
		WHERE push_subscriptions.user_id = excluded.user_id
		   OR (push_subscriptions.p256dh = excluded.p256dh AND push_subscriptions.auth = excluded.auth)`,
		userID, in.Endpoint, in.Keys.P256dh, in.Keys.Auth, userAgent)
	if err != nil {
		return fmt.Errorf("save subscription: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSubscriptionTaken
	}
	_, err = db.Exec(ctx, `
		DELETE FROM push_subscriptions WHERE id IN (
			SELECT id FROM push_subscriptions WHERE user_id = $1
			ORDER BY updated_at DESC, created_at DESC OFFSET $2)`, userID, MaxSubscriptionsPerUser)
	return err
}

func (a *API) subscribe(w http.ResponseWriter, r *http.Request) {
	if a.VAPIDPublicKey == "" {
		writeErr(w, http.StatusNotFound, "Notifikasi push belum aktif di server ini.")
		return
	}
	userID, ok := a.viewer(w, r)
	if !ok {
		return
	}
	var in SubscriptionInput
	if !decodeBody(w, r, &in) {
		return
	}
	if err := a.validate(in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := SaveSubscription(r.Context(), a.DB, userID, in, r.UserAgent()); err != nil {
		if errors.Is(err, ErrSubscriptionTaken) {
			writeErr(w, http.StatusConflict, ErrSubscriptionTaken.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "Gagal menyimpan langganan notifikasi. Coba lagi, ya.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (a *API) unsubscribe(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.viewer(w, r)
	if !ok {
		return
	}
	var in struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.Endpoint == "" {
		writeErr(w, http.StatusBadRequest, "Endpoint langganan wajib diisi.")
		return
	}
	tag, err := a.DB.Exec(r.Context(), `DELETE FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2`, userID, in.Endpoint)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Gagal menghapus langganan. Coba lagi, ya.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": tag.RowsAffected() > 0})
}

// PrefsResponse is the body of GET/PUT /api/v1/me/notification-prefs.
type PrefsResponse struct {
	OK                bool             `json:"ok"`
	Prefs             Prefs            `json:"prefs"`
	Available         map[Channel]bool `json:"available"`
	PushSubscriptions int              `json:"pushSubscriptions"`
}

func (a *API) prefsResponse(ctx context.Context, userID string, p Prefs) (PrefsResponse, error) {
	avail := map[Channel]bool{ChannelInApp: true}
	for _, ch := range QueuedChannels {
		avail[ch] = a.Available[ch]
	}
	res := PrefsResponse{OK: true, Prefs: p, Available: avail}
	err := a.DB.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE user_id = $1`, userID).Scan(&res.PushSubscriptions)
	return res, err
}

func (a *API) getPrefs(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.viewer(w, r)
	if !ok {
		return
	}
	p, err := LoadPrefs(r.Context(), a.DB, userID)
	if err == nil {
		var res PrefsResponse
		if res, err = a.prefsResponse(r.Context(), userID, p); err == nil {
			writeJSON(w, http.StatusOK, res)
			return
		}
	}
	writeErr(w, http.StatusInternalServerError, "Gagal memuat pengaturan notifikasi.")
}

func (a *API) putPrefs(w http.ResponseWriter, r *http.Request) {
	userID, ok := a.viewer(w, r)
	if !ok {
		return
	}
	var patch PrefsPatch
	if !decodeBody(w, r, &patch) {
		return
	}
	p, err := LoadPrefs(r.Context(), a.DB, userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Gagal memuat pengaturan notifikasi.")
		return
	}
	if err := patch.Apply(&p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := SavePrefs(r.Context(), a.DB, userID, p); err != nil {
		var ue UserError
		if errors.As(err, &ue) {
			writeErr(w, http.StatusBadRequest, ue.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "Gagal menyimpan pengaturan notifikasi.")
		return
	}
	res, err := a.prefsResponse(r.Context(), userID, p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Gagal memuat pengaturan notifikasi.")
		return
	}
	writeJSON(w, http.StatusOK, res)
}
