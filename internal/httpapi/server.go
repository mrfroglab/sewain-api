// Package httpapi exposes the Sewain API: dataset snapshot, actions (the booking state machine & friends),
// photo evidence upload, PostGIS search, demo controls and a WebSocket change feed.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrfroglab/sewain-api/internal/config"
	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/seed"
	"github.com/mrfroglab/sewain-api/internal/store"
)

type Server struct {
	cfg    config.Config
	eng    *domain.Engine
	db     *store.Store
	hub    *Hub
	log    *slog.Logger
	tokens sync.Map // token → userID cache
}

func New(cfg config.Config, eng *domain.Engine, db *store.Store, log *slog.Logger) *Server {
	return &Server{cfg: cfg, eng: eng, db: db, hub: NewHub(), log: log}
}

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
					w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
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

// viewer resolves the bearer token to a user (nil for guests).
func (s *Server) viewer(r *http.Request) *domain.User {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == r.Header.Get("Authorization") {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		return nil
	}
	uid, ok := s.tokens.Load(token)
	if !ok {
		id, err := s.db.SessionUser(r.Context(), token)
		if err != nil || id == "" {
			return nil
		}
		s.tokens.Store(token, id)
		uid = id
	}
	s.eng.Lock()
	defer s.eng.Unlock()
	if u := s.eng.User(uid.(string)); u != nil {
		cp := *u
		return &cp
	}
	return nil
}

// commit persists whatever the last mutation changed and notifies every device. Caller holds the engine lock.
func (s *Server) commit(ctx context.Context) error {
	ch := s.eng.TakeChanges()
	if ch.Empty() {
		return nil
	}
	if err := s.db.Persist(ctx, s.eng.Data(), ch); err != nil {
		return err
	}
	msg, _ := json.Marshal(map[string]any{"type": "changed", "version": s.eng.Data().Version})
	go s.hub.Broadcast(msg)
	return nil
}

/* ---------------------------------------------------------------- handlers */

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	db := s.db.Ping(ctx) == nil
	status := http.StatusOK
	if !db {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ok": db, "db": db, "demoMode": s.cfg.DemoMode, "clients": s.hub.Count()})
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	s.eng.Lock()
	b, err := s.eng.Snapshot()
	s.eng.Unlock()
	if err != nil {
		writeErr(w, 500, "Gagal memuat data.")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// demoLogin issues a session for any user (DEMO_MODE only: OTP is simulated in the prototype, PRD §7.2).
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
	s.eng.Lock()
	u := s.eng.User(in.UserID)
	s.eng.Unlock()
	if u == nil {
		writeErr(w, 404, "Akun tidak ditemukan.")
		return
	}
	token := newToken()
	if err := s.db.SaveSession(r.Context(), token, u.ID); err != nil {
		writeErr(w, 500, "Gagal membuat sesi.")
		return
	}
	s.tokens.Store(token, u.ID)
	writeJSON(w, 200, map[string]any{"ok": true, "token": token, "userId": u.ID})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := s.viewer(r)
	if u == nil {
		writeErr(w, 401, "Belum masuk.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": u})
}

// action runs one web-store action on the server: POST /api/v1/actions/{name} {"args": [...], "id": "<client id>"}.
func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in struct {
		Args []json.RawMessage `json:"args"`
		ID   string            `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, 400, "Body tidak valid.")
		return
	}
	if domain.ClientOnly[name] {
		writeJSON(w, 200, map[string]any{"ok": true, "ignored": true})
		return
	}
	viewer := s.viewer(r)
	ctx := r.Context()

	// Demo controls (DEMO_MODE only).
	switch name {
	case "advanceClock", "resetClock", "resetDemo":
		if !s.cfg.DemoMode {
			writeErr(w, 403, "Hanya tersedia di DEMO_MODE.")
			return
		}
		if viewer == nil {
			writeErr(w, 401, "Masuk dulu untuk memakai kontrol demo.")
			return
		}
		s.eng.Lock()
		defer s.eng.Unlock()
		switch name {
		case "advanceClock":
			var ms int64
			if len(in.Args) > 0 {
				_ = json.Unmarshal(in.Args[0], &ms)
			}
			if ms <= 0 || ms > 30*domain.Day {
				writeErr(w, 400, "Lompatan waktu tidak valid.")
				return
			}
			s.eng.AdvanceClock(ms)
		case "resetClock":
			s.eng.ResetClock()
		case "resetDemo":
			if err := s.resetLocked(ctx); err != nil {
				s.log.Error("reset", "err", err)
				writeErr(w, 500, "Gagal reset data.")
				return
			}
			writeJSON(w, 200, map[string]any{"ok": true, "version": s.eng.Data().Version})
			return
		}
		if err := s.commit(ctx); err != nil {
			writeErr(w, 500, "Gagal menyimpan.")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "version": s.eng.Data().Version})
		return
	}

	s.eng.Lock()
	defer s.eng.Unlock()
	value, err := s.eng.Dispatch(viewer, name, in.Args, in.ID)
	var uerr domain.UserError
	switch {
	case errors.Is(err, domain.ErrForbidden):
		_ = s.eng.TakeChanges() // nothing should have changed, but never leak partial state
		writeErr(w, 403, "Sesi tidak cocok dengan akun ini. Masuk ulang, ya.")
		return
	case errors.Is(err, domain.ErrUnknownAction):
		writeErr(w, 404, "Aksi tidak dikenal: "+name)
		return
	case errors.As(err, &uerr):
		// Rule violations happen before any mutation, but persist defensively anyway.
		_ = s.commit(ctx)
		writeJSON(w, 200, map[string]any{"ok": false, "error": uerr.Error(), "version": s.eng.Data().Version})
		return
	case err != nil:
		writeErr(w, 500, "Terjadi kesalahan.")
		return
	}
	if err := s.commit(ctx); err != nil {
		s.log.Error("persist", "action", name, "err", err)
		writeErr(w, 500, "Gagal menyimpan ke database.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "value": value, "version": s.eng.Data().Version})
}

// resetLocked rebuilds the demo dataset at "now" (caller holds the engine lock).
func (s *Server) resetLocked(ctx context.Context) error {
	d, err := seed.Build(time.Now().UnixMilli())
	if err != nil {
		return err
	}
	d.Version = s.eng.Data().Version + 1
	s.eng.Replace(d)
	if err := s.db.ReplaceAll(ctx, d); err != nil {
		return err
	}
	msg, _ := json.Marshal(map[string]any{"type": "changed", "version": d.Version, "reset": true})
	go s.hub.Broadcast(msg)
	return nil
}

// Seed resets the dataset on first boot.
func (s *Server) Seed(ctx context.Context) error {
	s.eng.Lock()
	defer s.eng.Unlock()
	return s.resetLocked(ctx)
}

var photoStages = map[string]bool{"referensi": true, "checkout": true, "checkin": true, "jaminan": true, "masalah": true, "chat": true, "sengketa": true}

// uploadPhoto stores evidence: SHA-256 is computed over the bytes the server received and the timestamp comes
// from the server clock (FR-P2/P3, FR-P7). KTP/selfie are never uploaded in the prototype (PRD §7.2).
func (s *Server) uploadPhoto(w http.ResponseWriter, r *http.Request) {
	viewer := s.viewer(r)
	if viewer == nil {
		writeErr(w, 401, "Masuk dulu untuk mengunggah foto.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeErr(w, 400, "Foto terlalu besar atau tidak valid (maks 10 MB).")
		return
	}
	stage := r.FormValue("stage")
	if !photoStages[stage] {
		writeErr(w, 400, "Tahap foto tidak valid.")
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "File foto wajib.")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		writeErr(w, 400, "Gagal membaca foto.")
		return
	}
	ctype := http.DetectContentType(data)
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif"}[ctype]
	if ext == "" {
		writeErr(w, 415, "Format foto tidak didukung ("+ctype+").")
		return
	}
	sum := sha256.Sum256(data)
	id := domain.NewID("p")
	if err := os.MkdirAll(s.cfg.UploadDir, 0o755); err != nil {
		writeErr(w, 500, "Penyimpanan foto tidak tersedia.")
		return
	}
	if err := os.WriteFile(filepath.Join(s.cfg.UploadDir, id+ext), data, 0o644); err != nil {
		writeErr(w, 500, "Gagal menyimpan foto.")
		return
	}
	source := r.FormValue("source")
	if source != "kamera" && source != "galeri" {
		source = "galeri"
	}
	rec := &domain.PhotoRecord{ID: id, Stage: stage, UploaderID: viewer.ID, Src: "/api/v1/photos/" + id, Sha256: hex.EncodeToString(sum[:]), Source: source, Bytes: ptrInt64(int64(len(data)))}
	if v := r.FormValue("bookingId"); v != "" {
		rec.BookingID = &v
	}
	if v := r.FormValue("angle"); v != "" {
		rec.Angle = &v
	}
	if v, err := strconv.Atoi(r.FormValue("width")); err == nil && v > 0 {
		rec.Width = &v
	}
	if v, err := strconv.Atoi(r.FormValue("height")); err == nil && v > 0 {
		rec.Height = &v
	}
	_ = hdr
	s.eng.Lock()
	defer s.eng.Unlock()
	rec.ServerTimestamp = s.eng.Now()
	s.eng.AddPhoto(rec)
	if err := s.commit(r.Context()); err != nil {
		writeErr(w, 500, "Gagal menyimpan data foto.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "photo": rec})
}

func ptrInt64(v int64) *int64 { return &v }

// demoPhoto registers a bundled sample image ("Pakai foto contoh") as evidence, hashed by the server.
func (s *Server) demoPhoto(w http.ResponseWriter, r *http.Request) {
	viewer := s.viewer(r)
	if viewer == nil {
		writeErr(w, 401, "Masuk dulu.")
		return
	}
	var in struct {
		URL       string  `json:"url"`
		Stage     string  `json:"stage"`
		BookingID *string `json:"bookingId"`
		Angle     *string `json:"angle"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil || !strings.HasPrefix(in.URL, "/img/") || strings.Contains(in.URL, "..") {
		writeErr(w, 400, "URL foto contoh tidak valid.")
		return
	}
	if !photoStages[in.Stage] {
		writeErr(w, 400, "Tahap foto tidak valid.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.WebOrigin+in.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		writeErr(w, 502, "Foto contoh tidak bisa diambil.")
		return
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 10<<20))
	sum := sha256.Sum256(data)
	rec := &domain.PhotoRecord{ID: domain.NewID("p"), BookingID: in.BookingID, Stage: in.Stage, UploaderID: viewer.ID, Src: in.URL, Sha256: hex.EncodeToString(sum[:]), Source: "demo", Angle: in.Angle, Bytes: ptrInt64(int64(len(data)))}
	s.eng.Lock()
	defer s.eng.Unlock()
	rec.ServerTimestamp = s.eng.Now()
	s.eng.AddPhoto(rec)
	if err := s.commit(r.Context()); err != nil {
		writeErr(w, 500, "Gagal menyimpan data foto.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "photo": rec})
}

func (s *Server) servePhoto(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.eng.Lock()
	p := s.eng.Photo(id)
	s.eng.Unlock()
	if p == nil || !strings.HasPrefix(p.Src, "/api/v1/photos/") {
		http.NotFound(w, r)
		return
	}
	matches, _ := filepath.Glob(filepath.Join(s.cfg.UploadDir, filepath.Base(id)+".*"))
	if len(matches) == 0 {
		http.NotFound(w, r)
		return
	}
	// Evidence never changes (append-only), so it can be cached forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-SHA256", p.Sha256)
	http.ServeFile(w, r, matches[0])
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := func(k string) float64 { v, _ := strconv.ParseFloat(q.Get(k), 64); return v }
	i := func(k string) int64 { v, _ := strconv.ParseInt(q.Get(k), 10, 64); return v }
	p := store.SearchParams{Lat: f("lat"), Lng: f("lng"), RadiusKm: f("radiusKm"), Category: q.Get("kategori"), MinPrice: i("min"), MaxPrice: i("max"),
		Handover: q.Get("serah"), Collateral: q.Get("jaminan"), ProviderType: q.Get("penyedia"), Q: q.Get("q")}
	if p.Lat == 0 || p.Lng == 0 || p.RadiusKm <= 0 || p.RadiusKm > 50 {
		writeErr(w, 400, "lat, lng dan radiusKm (≤ 50) wajib.")
		return
	}
	res, err := s.db.Search(r.Context(), p)
	if err != nil {
		s.log.Error("search", "err", err)
		writeErr(w, 500, "Pencarian gagal.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "hits": res.Hits, "hiddenByProviderReach": res.HiddenByProviderReach, "query": res.Query})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	s.eng.Lock()
	v := s.eng.Data().Version
	s.eng.Unlock()
	hello, _ := json.Marshal(map[string]any{"type": "hello", "version": v})
	origins := []string{"*"}
	s.hub.Serve(w, r, origins, hello)
}

// RunTicker runs the time-based jobs (expiry, overdue, auto-complete, L1→L2) every interval.
func (s *Server) RunTicker(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.eng.Lock()
			s.eng.Tick()
			if err := s.commit(ctx); err != nil {
				s.log.Error("tick persist", "err", err)
			}
			s.eng.Unlock()
		}
	}
}

var _ = fmt.Sprintf
