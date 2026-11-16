// Package httpapi exposes the Sewain API: per-viewer dataset snapshot, actions (each one a database transaction),
// photo evidence, PostGIS search, OTP sessions, KYC, push, account export/deletion, admin tools and the per-user
// WebSocket change feed. The snapshot + action contract is the one the web app (sewain-web) has used since v1.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/mrfroglab/sewain-api/internal/auth"
	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/kyc"
	"github.com/mrfroglab/sewain-api/internal/media"
	"github.com/mrfroglab/sewain-api/internal/notify"
	"github.com/mrfroglab/sewain-api/internal/platform/health"
	"github.com/mrfroglab/sewain-api/internal/platform/metrics"
	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
	"github.com/mrfroglab/sewain-api/internal/platform/ratelimit"
	"github.com/mrfroglab/sewain-api/internal/realtime"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// Deps are the modules the server routes to. All are required except Metrics (nil = no instrumentation).
type Deps struct {
	Auth     *auth.Service
	KYC      *kyc.Service
	Storage  media.Storage
	Signer   *media.Signer
	Hub      *realtime.Hub
	Notify   *notify.API
	Limiter  ratelimit.Limiter
	Metrics  *metrics.Metrics
	Ready    health.Readiness
	Profiles RateProfiles
	// IPResolver resolves the client IP behind trusted proxies (TRUSTED_PROXY_CIDRS).
	IPResolver *middleware.IPResolver
}

// RateProfiles are the per-route rate limits (docs/integration/ops.md §2). Zero values use the ratelimit defaults.
type RateProfiles struct {
	IP, Public, Write, Upload, Auth, Demo ratelimit.Profile
}

// ProfilesFromRates builds the built-in profiles (names and keys) with the configured rates (RATE_* env).
func ProfilesFromRates(r ratelimit.Rates) RateProfiles {
	r = r.WithDefaults()
	return RateProfiles{
		IP:     ratelimit.IP.With(r.IP),
		Public: ratelimit.Public.With(r.Public),
		Write:  ratelimit.Write.With(r.Write),
		Upload: ratelimit.Upload.With(r.Upload),
		Auth:   ratelimit.Auth.With(r.Auth),
		Demo:   ratelimit.Demo.With(r.Demo),
	}
}

func (p RateProfiles) withDefaults() RateProfiles {
	if p.IP.Name == "" {
		p.IP = ratelimit.IP
	}
	if p.Public.Name == "" {
		p.Public = ratelimit.Public
	}
	if p.Write.Name == "" {
		p.Write = ratelimit.Write
	}
	if p.Upload.Name == "" {
		p.Upload = ratelimit.Upload
	}
	if p.Auth.Name == "" {
		p.Auth = ratelimit.Auth
	}
	if p.Demo.Name == "" {
		p.Demo = ratelimit.Demo
	}
	return p
}

// Server holds the handlers.
type Server struct {
	cfg  config.Config
	svc  *service.Service
	log  *slog.Logger
	deps Deps
}

// New builds the server.
func New(cfg config.Config, svc *service.Service, log *slog.Logger, deps Deps) *Server {
	deps.Profiles = deps.Profiles.withDefaults()
	return &Server{cfg: cfg, svc: svc, log: log, deps: deps}
}

// photoPath is the canonical (unsigned) URL of a stored photo.
const photoPath = "/api/v1/photos/"

// Routes returns the whole API with the platform middleware chain (docs/integration/ops.md §1).
func (s *Server) Routes() http.Handler {
	d := s.deps
	rl := func(p ratelimit.Profile) func(http.Handler) http.Handler {
		return ratelimit.Middleware(d.Limiter, p, ratelimit.Options{Log: s.log, OnLimited: func(profile string, _ *http.Request) {
			if d.Metrics != nil {
				d.Metrics.RateLimited(profile)
			}
		}})
	}
	write, upload, authRL, demoRL := rl(d.Profiles.Write), rl(d.Profiles.Upload), rl(d.Profiles.Auth), rl(d.Profiles.Demo)
	user, admin := auth.RequireUser, auth.RequireAdmin
	fn := func(f http.HandlerFunc) http.Handler { return f }

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.Handle("GET /api/ready", d.Ready.Handler())
	mux.HandleFunc("GET /api/v1/config", s.publicConfig)
	mux.HandleFunc("GET /api/v1/state", s.state)
	mux.HandleFunc("GET /api/v1/listings/search", s.search)
	mux.HandleFunc("GET /api/v1/users/{id}", s.publicUser)
	mux.Handle("GET /api/v1/events", d.Hub)

	// Auth (OTP, sessions) and the DEMO_MODE persona login.
	ah := auth.Handlers{S: d.Auth}
	mux.Handle("POST /api/v1/auth/otp/request", authRL(fn(ah.OTPRequest)))
	mux.Handle("POST /api/v1/auth/otp/verify", authRL(fn(ah.OTPVerify)))
	mux.Handle("POST /api/v1/auth/demo-login", demoRL(fn(s.demoLogin)))
	mux.Handle("POST /api/v1/auth/logout", write(fn(ah.Logout)))
	mux.HandleFunc("GET /api/v1/auth/me", s.me)
	mux.Handle("GET /api/v1/auth/sessions", user(fn(ah.ListSessions)))
	mux.Handle("DELETE /api/v1/auth/sessions/{id}", write(user(fn(ah.RevokeSession))))
	mux.Handle("DELETE /api/v1/auth/sessions", write(user(fn(ah.RevokeOtherSessions))))

	// KYC (encrypted KTP + selfie, admin queue with audited file access).
	kh := kyc.Handlers{S: d.KYC}
	mux.Handle("POST /api/v1/kyc/submissions", upload(user(fn(kh.Submit))))
	mux.Handle("GET /api/v1/kyc/status", user(fn(kh.Status)))
	mux.Handle("GET /api/v1/admin/kyc", admin(fn(kh.Queue)))
	mux.Handle("GET /api/v1/admin/kyc/{id}", admin(fn(kh.GetOne)))
	mux.Handle("POST /api/v1/admin/kyc/{id}/decision", write(admin(fn(kh.Decide))))
	mux.Handle("GET /api/v1/admin/kyc/{id}/files/{kind}", admin(fn(kh.File)))
	mux.Handle("POST /api/v1/admin/kyc/blacklist", write(admin(fn(kh.Blacklist))))

	// Actions and photos.
	mux.Handle("POST /api/v1/actions/{name}", write(fn(s.action)))
	mux.Handle("POST /api/v1/photos", upload(fn(s.uploadPhoto)))
	mux.Handle("POST /api/v1/photos/demo", upload(fn(s.demoPhoto)))
	photos := &media.Handler{Storage: d.Storage, Signer: d.Signer, Lookup: s.svc, Log: s.log,
		SessionViewer: func(r *http.Request) *media.Viewer { return mediaViewer(viewerOf(r)) }}
	mux.Handle("GET /api/v1/photos/{id}", photos)
	mux.Handle("HEAD /api/v1/photos/{id}", photos)

	// Push and notification preferences (notify module).
	nmux := http.NewServeMux()
	d.Notify.Register(nmux)
	mux.Handle("GET /api/v1/push/vapid-key", nmux)
	mux.Handle("POST /api/v1/push/subscriptions", write(nmux))
	mux.Handle("DELETE /api/v1/push/subscriptions", write(nmux))
	mux.Handle("GET /api/v1/me/notification-prefs", nmux)
	mux.Handle("PUT /api/v1/me/notification-prefs", write(nmux))

	// Account (UU PDP) and admin tools.
	mux.Handle("GET /api/v1/me/export", user(fn(s.exportMe)))
	mux.Handle("DELETE /api/v1/me", write(user(fn(s.deleteMe))))
	mux.Handle("GET /api/v1/admin/metrics", admin(fn(s.adminMetrics)))
	mux.Handle("POST /api/v1/admin/listings/import", write(admin(fn(s.importListings))))

	var inner http.Handler = mux
	if d.Metrics != nil {
		inner = d.Metrics.Instrument(mux) // must wrap the mux directly (route label)
	}
	ipRes := d.IPResolver
	if ipRes == nil {
		ipRes, _ = middleware.NewIPResolver(nil)
	}
	cors := middleware.DefaultCORS(s.cfg.CORSOrigins)
	cors.AllowedHeaders = append(cors.AllowedHeaders, "If-None-Match")
	cors.ExposedHeaders = append(cors.ExposedHeaders, "Idempotency-Replayed", "X-Original-SHA256", "X-RateLimit-Limit", "X-RateLimit-Remaining")
	for _, o := range s.cfg.CORSOrigins {
		if o == "*" { // demo only (refused in production): browsers reject "*" with credentials
			cors.AllowCredentials = false
		}
	}
	return middleware.Chain(inner,
		middleware.RequestID,
		middleware.RealIP(ipRes),
		middleware.Logger(s.log, middleware.LoggerOptions{Skip: func(r *http.Request) bool { return r.URL.Path == "/api/health" }}),
		middleware.Recover(s.log),
		middleware.SecurityHeaders,
		middleware.CORS(cors),
		middleware.BodyLimit(middleware.DefaultBodyLimit,
			middleware.BodyRule{Method: http.MethodPost, Path: "/api/v1/photos", Limit: middleware.UploadBodyLimit},
			middleware.BodyRule{Method: http.MethodPost, Path: "/api/v1/kyc/submissions", Limit: 2*kyc.MaxFileBytes + 1<<20},
			middleware.BodyRule{Method: http.MethodPost, Path: "/api/v1/admin/listings/import", Limit: importBodyLimit},
		),
		middleware.Timeout(s.cfg.RequestTimeout, middleware.TimeoutOptions{}),
		clientAddr,
		s.wsQueryToken,
		// Cheap per-IP brake before any session lookup (review wave 2, L6): made-up tokens from one address stop
		// here. Generous enough for a campus NAT.
		rl(d.Profiles.IP),
		d.Auth.Middleware, // also records the user for logs and per-user rate limits (middleware.SetUserID)
		// The coarse limit on every route runs after the viewer is resolved so signed-in students behind one campus
		// NAT each get their own bucket; guests (and invalid tokens) share the bucket of their IP.
		rl(d.Profiles.Public),
	)
}

/* --------------------------------------------------------------- plumbing */

// clientAddr hands the resolved client IP (trusted-proxy aware) to code that reads r.RemoteAddr, such as the auth
// module's per-IP OTP limits and session metadata.
func clientAddr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := middleware.ClientIPFrom(r.Context()); ip != "" {
			r2 := r.Clone(r.Context())
			r2.RemoteAddr = ip + ":0"
			if strings.Contains(ip, ":") {
				r2.RemoteAddr = "[" + ip + "]:0"
			}
			r = r2
		}
		next.ServeHTTP(w, r)
	})
}

// wsQueryToken: browsers cannot send headers on a WebSocket, so in bearer mode (per-tab demo sessions,
// SESSION_BEARER_LOGIN) the events endpoint accepts the token as ?token=. Never in production.
func (s *Server) wsQueryToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Auth.AllowBearerLogin && r.URL.Path == "/api/v1/events" && r.Header.Get("Authorization") == "" {
			if tok := r.URL.Query().Get("token"); tok != "" {
				r = r.Clone(r.Context())
				r.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// viewerOf is the signed-in user of a request (nil = guest), as the core service sees it.
func viewerOf(r *http.Request) *service.Viewer {
	v := auth.ViewerFrom(r.Context())
	if v.IsGuest() {
		return nil
	}
	role := "user"
	if v.IsAdmin() {
		role = "admin"
	}
	return &service.Viewer{UserID: v.UserID, Role: role}
}

func mediaViewer(v *service.Viewer) *media.Viewer {
	if v == nil {
		return nil
	}
	return &media.Viewer{ID: v.UserID, Admin: v.IsAdmin()}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(bytes.TrimRight(body, "\n"), '\n'))
}

/* ---------------------------------------------------------------- handlers */

// health is liveness (no database): build info plus the v1 fields the old web still reads.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": health.Version, "commit": health.Commit,
		"demoMode": s.cfg.DemoMode, "clients": s.deps.Hub.Count()})
}

// publicConfig is GET /api/v1/config: what the web needs to pick its flows (no auth, cacheable for a minute).
func (s *Server) publicConfig(w http.ResponseWriter, _ *http.Request) {
	var push any
	if k := s.cfg.Notify.VAPID.PublicKey; s.cfg.Notify.VAPID.Enabled() {
		push = map[string]any{"vapidPublicKey": k}
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "appEnv": s.cfg.AppEnv, "demoMode": s.cfg.DemoMode, "paymentMode": s.svc.PaymentMode(),
		"auth":  map[string]any{"otpProvider": s.cfg.Auth.OTPProvider},
		"push":  push,
		"legal": map[string]any{"termsVersion": s.cfg.Auth.TermsVersion, "privacyVersion": s.cfg.Auth.PrivacyVersion},
	})
}

// demoLogin issues a bearer session for any user (DEMO_MODE persona picker, PRD §7.2). It is a real auth session
// (hashed token, sliding expiry, listed under /auth/sessions).
func (s *Server) demoLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DemoMode {
		writeErr(w, http.StatusForbidden, "Login demo hanya tersedia di DEMO_MODE.")
		return
	}
	var in struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "Body tidak valid.")
		return
	}
	u, err := s.svc.GetUserAuth(r.Context(), in.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Gagal membuat sesi.")
		return
	}
	if u == nil {
		writeErr(w, http.StatusNotFound, "Akun tidak ditemukan.")
		return
	}
	token, _, err := s.deps.Auth.CreateSession(r.Context(), u.ID, s.deps.Auth.Meta(r))
	if err != nil {
		s.log.Error("demo login", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal membuat sesi.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "userId": u.ID})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	viewer := viewerOf(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "Belum masuk.")
		return
	}
	u, err := s.svc.User(r.Context(), viewer.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Gagal memuat akun.")
		return
	}
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "Belum masuk.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u})
}

// actionResponse is the v1 response body of POST /api/v1/actions/{name}.
func actionResponse(name string, res service.Result, err error) (int, json.RawMessage) {
	var body any
	status := http.StatusOK
	switch {
	case err == nil:
		body = map[string]any{"ok": true, "value": res.Value, "version": res.Version}
	case errors.Is(err, domain.ErrForbidden):
		status, body = http.StatusForbidden, map[string]any{"ok": false, "error": "Sesi tidak cocok dengan akun ini. Masuk ulang, ya."}
	case errors.Is(err, domain.ErrUnknownAction):
		status, body = http.StatusNotFound, map[string]any{"ok": false, "error": "Aksi tidak dikenal: " + name}
	default:
		if msg, ok := service.IsUserError(err); ok {
			body = map[string]any{"ok": false, "error": msg, "version": res.Version}
		} else {
			status, body = http.StatusInternalServerError, map[string]any{"ok": false, "error": "Terjadi kesalahan."}
		}
	}
	b, _ := json.Marshal(body)
	return status, b
}

// action runs one web-store action on the server: POST /api/v1/actions/{name} {"args": [...], "id": "<client id>",
// "idempotencyKey": "<uuid>"}. With an Idempotency-Key (header, or the body field when the header would trigger a
// CORS preflight; the header wins) the action runs at most once per key and repeats return the stored response.
func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		if middleware.IsBodyTooLarge(err) {
			middleware.RespondTooLarge(w)
			return
		}
		writeErr(w, http.StatusBadRequest, "Body tidak valid.")
		return
	}
	var in struct {
		Args           []json.RawMessage `json:"args"`
		ID             string            `json:"id"`
		IdempotencyKey string            `json:"idempotencyKey"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "Body tidak valid.")
		return
	}
	if domain.ClientOnly[name] {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	viewer := viewerOf(r)
	ctx := r.Context()

	switch name {
	case "advanceClock", "resetClock", "resetDemo":
		s.demoControl(w, r, viewer, name, in.Args)
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		key = strings.TrimSpace(in.IdempotencyKey)
	}
	if key != "" && viewer != nil {
		if len(key) > 200 {
			writeErr(w, http.StatusBadRequest, "Idempotency-Key terlalu panjang.")
			return
		}
		out, replayed, err := s.svc.DispatchIdempotent(ctx, viewer, key, service.RequestHash(name, canonicalBody(in.Args, in.ID)), name, in.Args, in.ID,
			func(res service.Result, err error) (int, json.RawMessage) { return actionResponse(name, res, err) })
		if errors.Is(err, service.ErrIdempotencyMismatch) {
			writeErr(w, http.StatusConflict, "Idempotency-Key ini sudah dipakai untuk permintaan lain.")
			return
		}
		if err != nil {
			s.log.Error("action", "action", name, "err", err)
			writeErr(w, http.StatusInternalServerError, "Gagal menyimpan ke database.")
			return
		}
		if replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeRaw(w, out.Status, out.Body)
		return
	}

	res, err := s.svc.Dispatch(ctx, viewer, name, in.Args, in.ID)
	if err != nil {
		if _, isRule := service.IsUserError(err); isRule {
			res.Version, _ = s.svc.Version(ctx)
		} else if !errors.Is(err, domain.ErrForbidden) && !errors.Is(err, domain.ErrUnknownAction) {
			s.log.Error("action", "action", name, "err", err)
			writeErr(w, http.StatusInternalServerError, "Gagal menyimpan ke database.")
			return
		}
	}
	status, body := actionResponse(name, res, err)
	writeRaw(w, status, body)
}

// canonicalBody is what an idempotency key fingerprints: the arguments and the client id, not the key field itself
// (a retry may move the key from the body to the header).
func canonicalBody(args []json.RawMessage, id string) []byte {
	b, _ := json.Marshal(map[string]any{"args": args, "id": id})
	return b
}

// demoControl: advanceClock / resetClock / resetDemo (DEMO_MODE only, any signed-in user, like v1).
func (s *Server) demoControl(w http.ResponseWriter, r *http.Request, viewer *service.Viewer, name string, args []json.RawMessage) {
	if !s.cfg.DemoMode {
		writeErr(w, http.StatusForbidden, "Hanya tersedia di DEMO_MODE.")
		return
	}
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "Masuk dulu untuk memakai kontrol demo.")
		return
	}
	ctx := r.Context()
	var ch service.Change
	var err error
	switch name {
	case "advanceClock":
		var ms int64
		if len(args) > 0 {
			_ = json.Unmarshal(args[0], &ms)
		}
		if ms <= 0 || ms > service.MaxClockJump {
			writeErr(w, http.StatusBadRequest, "Lompatan waktu tidak valid.")
			return
		}
		ch, err = s.svc.AdvanceClock(ctx, ms)
	case "resetClock":
		ch, err = s.svc.ResetClock(ctx)
	case "resetDemo":
		ch, err = s.svc.ResetDemo(ctx)
	}
	if err != nil {
		s.log.Error("demo control", "action", name, "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal menyimpan.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": ch.Version})
}

// publicUser is GET /api/v1/users/{id}: the public profile of one user (the users[] entry of a guest snapshot), their
// active listings and the reviews about them that are shown. The same for every viewer, so publicly cacheable.
func (s *Server) publicUser(w http.ResponseWriter, r *http.Request) {
	page, err := s.svc.PublicUser(r.Context(), r.PathValue("id"))
	if err != nil {
		s.log.Error("public user", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal memuat profil.")
		return
	}
	if page == nil {
		writeErr(w, http.StatusNotFound, "Pengguna tidak ditemukan.")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": page.User, "listings": page.Listings, "reviews": page.Reviews})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := func(k string) float64 { v, _ := strconv.ParseFloat(q.Get(k), 64); return v }
	i := func(k string) int64 { v, _ := strconv.ParseInt(q.Get(k), 10, 64); return v }
	p := service.SearchParams{Lat: f("lat"), Lng: f("lng"), RadiusKm: f("radiusKm"), Category: q.Get("kategori"), MinPrice: i("min"), MaxPrice: i("max"),
		Handover: q.Get("serah"), Collateral: q.Get("jaminan"), ProviderType: q.Get("penyedia"), Q: q.Get("q"),
		StartDate: q.Get("mulai"), EndDate: q.Get("selesai")}
	if p.Lat == 0 || p.Lng == 0 || p.RadiusKm <= 0 || p.RadiusKm > 50 {
		writeErr(w, http.StatusBadRequest, "lat, lng dan radiusKm (≤ 50) wajib.")
		return
	}
	res, err := s.svc.Search(r.Context(), p)
	if err != nil {
		s.log.Error("search", "err", err)
		writeErr(w, http.StatusInternalServerError, "Pencarian gagal.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hits": res.Hits, "hiddenByProviderReach": res.HiddenByProviderReach, "radiusKm": res.RadiusKm, "query": res.Query})
}
