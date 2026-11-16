// Package httpapi exposes the Sewain API: per-viewer dataset snapshot, actions (each one a database transaction),
// photo evidence upload, PostGIS search, demo controls and a WebSocket change feed. The HTTP contract is the one the
// web app (sewain-web) has used since v1.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

type Server struct {
	cfg    config.Config
	svc    *service.Service
	hub    *Hub
	log    *slog.Logger
	viewer ViewerResolver
	photos PhotoStore
}

// Option customises the server (integration points for the auth and media modules).
type Option func(*Server)

// WithViewerResolver replaces how requests are authenticated (default: DEMO_MODE bearer tokens).
func WithViewerResolver(v ViewerResolver) Option { return func(s *Server) { s.viewer = v } }

// WithPhotoStore replaces where photo bytes live (default: LocalPhotoStore{cfg.UploadDir}).
func WithPhotoStore(p PhotoStore) Option { return func(s *Server) { s.photos = p } }

func New(cfg config.Config, svc *service.Service, log *slog.Logger, opts ...Option) *Server {
	s := &Server{cfg: cfg, svc: svc, hub: NewHub(), log: log}
	s.viewer = DemoTokens{Svc: svc}
	s.photos = LocalPhotoStore{Dir: cfg.UploadDir}
	for _, o := range opts {
		o(s)
	}
	// Realtime: every committed change is announced. v1 behaviour (broadcast to every client); the notify module
	// replaces the hub with per-user fan-out based on Change.Users/Admins/Public.
	svc.OnChange(func(ch service.Change) {
		msg, _ := json.Marshal(map[string]any{"type": "changed", "version": ch.Version})
		go s.hub.Broadcast(msg)
	})
	return s
}

// Hub exposes the WebSocket hub (tests, metrics).
func (s *Server) Hub() *Hub { return s.hub }

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/v1/state", s.state)
	mux.HandleFunc("POST /api/v1/auth/demo-login", s.demoLogin)
	mux.HandleFunc("GET /api/v1/auth/me", s.me)
	mux.HandleFunc("POST /api/v1/actions/{name}", s.action)
	mux.HandleFunc("POST /api/v1/photos", s.uploadPhoto)
	mux.HandleFunc("POST /api/v1/photos/demo", s.demoPhoto)
	mux.HandleFunc("GET /api/v1/photos/{id}", s.servePhoto)
	mux.HandleFunc("GET /api/v1/listings/search", s.search)
	mux.HandleFunc("GET /api/v1/events", s.events)
	return s.cors(s.logRequests(mux))
}

/* --------------------------------------------------------------- plumbing */

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			for _, allowed := range s.cfg.CORSOrigins {
				if allowed == o || allowed == "*" {
					w.Header().Set("Access-Control-Allow-Origin", o)
					w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, If-None-Match")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
					w.Header().Set("Access-Control-Expose-Headers", "ETag, Idempotency-Replayed")
					w.Header().Set("Vary", "Origin")
				}
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

// Unwrap lets the websocket library reach the underlying Hijacker.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/events" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		if r.URL.Path != "/api/health" {
			s.log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

// viewerOr500 resolves the viewer; on a resolver error it answers 500 and returns ok=false.
func (s *Server) viewerOr500(w http.ResponseWriter, r *http.Request) (*service.Viewer, bool) {
	v, err := s.viewer.Resolve(r)
	if err != nil {
		s.log.Error("resolve viewer", "err", err)
		writeErr(w, 500, "Gagal memeriksa sesi.")
		return nil, false
	}
	return v, true
}

/* ---------------------------------------------------------------- handlers */

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	db := s.svc.Pool().Ping(ctx) == nil
	status := http.StatusOK
	if !db {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": db, "db": db, "demoMode": s.cfg.DemoMode, "clients": s.hub.Count()})
}

// state returns the viewer's snapshot (ROADMAP §4) with an ETag; If-None-Match → 304.
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.viewerOr500(w, r)
	if !ok {
		return
	}
	etag, _, err := s.svc.ETag(r.Context(), viewer)
	if err != nil {
		s.log.Error("etag", "err", err)
		writeErr(w, 500, "Gagal memuat data.")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "Authorization, Cookie")
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	snap, err := s.svc.Snapshot(r.Context(), viewer)
	if err != nil {
		s.log.Error("snapshot", "err", err)
		writeErr(w, 500, "Gagal memuat data.")
		return
	}
	b, err := service.SnapshotJSON(snap)
	if err != nil {
		writeErr(w, 500, "Gagal memuat data.")
		return
	}
	// The ETag describes the version the snapshot was built from (it may be newer than the one checked above),
	// so a 304 never hides a newer state.
	w.Header().Set("ETag", s.svc.ETagFor(viewer, snap.Version, snap.ClockOffset))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

// demoLogin issues a session for any user (DEMO_MODE only: OTP is simulated in the demo, PRD §7.2).
func (s *Server) demoLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DemoMode {
		writeErr(w, 403, "Login demo hanya tersedia di DEMO_MODE.")
		return
	}
	var in struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeErr(w, 400, "Body tidak valid.")
		return
	}
	u, err := s.svc.GetUserAuth(r.Context(), in.UserID)
	if err != nil {
		writeErr(w, 500, "Gagal membuat sesi.")
		return
	}
	if u == nil {
		writeErr(w, 404, "Akun tidak ditemukan.")
		return
	}
	token, err := s.svc.CreateDemoSession(r.Context(), u.ID)
	if err != nil {
		writeErr(w, 500, "Gagal membuat sesi.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "token": token, "userId": u.ID})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.viewerOr500(w, r)
	if !ok {
		return
	}
	if viewer == nil {
		writeErr(w, 401, "Belum masuk.")
		return
	}
	u, err := s.svc.User(r.Context(), viewer.UserID)
	if err != nil {
		writeErr(w, 500, "Gagal memuat akun.")
		return
	}
	if u == nil {
		writeErr(w, 401, "Belum masuk.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": u})
}

// actionResponse is the v1 response body of POST /api/v1/actions/{name}.
func actionResponse(name string, res service.Result, err error) (int, json.RawMessage) {
	var body any
	status := 200
	switch {
	case err == nil:
		body = map[string]any{"ok": true, "value": res.Value, "version": res.Version}
	case errors.Is(err, domain.ErrForbidden):
		status, body = 403, map[string]any{"ok": false, "error": "Sesi tidak cocok dengan akun ini. Masuk ulang, ya."}
	case errors.Is(err, domain.ErrUnknownAction):
		status, body = 404, map[string]any{"ok": false, "error": "Aksi tidak dikenal: " + name}
	default:
		if msg, ok := service.IsUserError(err); ok {
			body = map[string]any{"ok": false, "error": msg, "version": res.Version}
		} else {
			status, body = 500, map[string]any{"ok": false, "error": "Terjadi kesalahan."}
		}
	}
	b, _ := json.Marshal(body)
	return status, b
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(bytes.TrimRight(body, "\n"), '\n'))
}

// action runs one web-store action on the server: POST /api/v1/actions/{name} {"args": [...], "id": "<client id>"}.
// With an Idempotency-Key header the action runs at most once per key and repeats return the stored response.
func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, "Body tidak valid.")
		return
	}
	var in struct {
		Args []json.RawMessage `json:"args"`
		ID   string            `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		writeErr(w, 400, "Body tidak valid.")
		return
	}
	if domain.ClientOnly[name] {
		writeJSON(w, 200, map[string]any{"ok": true, "ignored": true})
		return
	}
	viewer, ok := s.viewerOr500(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	switch name {
	case "advanceClock", "resetClock", "resetDemo":
		s.demoControl(w, r, viewer, name, in.Args)
		return
	}

	if key := strings.TrimSpace(r.Header.Get("Idempotency-Key")); key != "" && viewer != nil {
		if len(key) > 200 {
			writeErr(w, 400, "Idempotency-Key terlalu panjang.")
			return
		}
		out, replayed, err := s.svc.DispatchIdempotent(ctx, viewer, key, service.RequestHash(name, raw), name, in.Args, in.ID,
			func(res service.Result, err error) (int, json.RawMessage) { return actionResponse(name, res, err) })
		if errors.Is(err, service.ErrIdempotencyMismatch) {
			writeErr(w, 422, "Idempotency-Key ini sudah dipakai untuk permintaan lain.")
			return
		}
		if err != nil {
			s.log.Error("action", "action", name, "err", err)
			writeErr(w, 500, "Gagal menyimpan ke database.")
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
			writeErr(w, 500, "Gagal menyimpan ke database.")
			return
		}
	}
	status, body := actionResponse(name, res, err)
	writeRaw(w, status, body)
}

// demoControl: advanceClock / resetClock / resetDemo (DEMO_MODE only, any signed-in user, like v1).
func (s *Server) demoControl(w http.ResponseWriter, r *http.Request, viewer *service.Viewer, name string, args []json.RawMessage) {
	if !s.cfg.DemoMode {
		writeErr(w, 403, "Hanya tersedia di DEMO_MODE.")
		return
	}
	if viewer == nil {
		writeErr(w, 401, "Masuk dulu untuk memakai kontrol demo.")
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
			writeErr(w, 400, "Lompatan waktu tidak valid.")
			return
		}
		ch, err = s.svc.AdvanceClock(ctx, ms)
	case "resetClock":
		ch, err = s.svc.ResetClock(ctx)
	case "resetDemo":
		ch, err = s.svc.ResetDemo(ctx)
		if err != nil {
			s.log.Error("reset", "err", err)
			writeErr(w, 500, "Gagal reset data.")
			return
		}
	}
	if err != nil {
		writeErr(w, 500, "Gagal menyimpan.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "version": ch.Version})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := func(k string) float64 { v, _ := strconv.ParseFloat(q.Get(k), 64); return v }
	i := func(k string) int64 { v, _ := strconv.ParseInt(q.Get(k), 10, 64); return v }
	p := service.SearchParams{Lat: f("lat"), Lng: f("lng"), RadiusKm: f("radiusKm"), Category: q.Get("kategori"), MinPrice: i("min"), MaxPrice: i("max"),
		Handover: q.Get("serah"), Collateral: q.Get("jaminan"), ProviderType: q.Get("penyedia"), Q: q.Get("q"),
		StartDate: q.Get("mulai"), EndDate: q.Get("selesai")}
	if p.Lat == 0 || p.Lng == 0 || p.RadiusKm <= 0 || p.RadiusKm > 50 {
		writeErr(w, 400, "lat, lng dan radiusKm (≤ 50) wajib.")
		return
	}
	res, err := s.svc.Search(r.Context(), p)
	if err != nil {
		s.log.Error("search", "err", err)
		writeErr(w, 500, "Pencarian gagal.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "hits": res.Hits, "hiddenByProviderReach": res.HiddenByProviderReach, "query": res.Query})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.Version(r.Context())
	if err != nil {
		writeErr(w, 500, "Gagal memuat versi.")
		return
	}
	hello, _ := json.Marshal(map[string]any{"type": "hello", "version": v})
	s.hub.Serve(w, r, []string{"*"}, hello)
}
