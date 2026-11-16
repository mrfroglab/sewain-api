package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// PhotoStore keeps the bytes of photo evidence. Everything that touches photo files goes through it, so the media
// module can swap in its media.Storage (local disk / R2, signed URLs) without touching the handlers.
type PhotoStore interface {
	// Put stores data under key (write-once).
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// Serve writes the object to w (sets Content-Type/caching headers). Returns false when the object is missing.
	Serve(w http.ResponseWriter, r *http.Request, key string) bool
}

// LocalPhotoStore is the v1 storage: one file per photo in a directory.
type LocalPhotoStore struct{ Dir string }

func (s LocalPhotoStore) Put(_ context.Context, key string, data []byte, _ string) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(s.Dir, filepath.Base(key))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // write-once
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (s LocalPhotoStore) Serve(w http.ResponseWriter, r *http.Request, key string) bool {
	path := filepath.Join(s.Dir, filepath.Base(key))
	if _, err := os.Stat(path); err != nil {
		return false
	}
	http.ServeFile(w, r, path)
	return true
}

var imageExt = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif"}

// uploadPhoto stores evidence: SHA-256 is computed over the bytes the server received and the timestamp comes
// from the server clock (FR-P2/P3, FR-P7).
func (s *Server) uploadPhoto(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.viewerOr500(w, r)
	if !ok {
		return
	}
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
	if !service.PhotoStages[stage] {
		writeErr(w, 400, "Tahap foto tidak valid.")
		return
	}
	f, _, err := r.FormFile("file")
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
	ext := imageExt[ctype]
	if ext == "" {
		writeErr(w, 415, "Format foto tidak didukung ("+ctype+").")
		return
	}
	sum := sha256.Sum256(data)
	id := domain.NewID("p")
	key := id + ext
	if err := s.photos.Put(r.Context(), key, data, ctype); err != nil {
		s.log.Error("photo store", "err", err)
		writeErr(w, 500, "Gagal menyimpan foto.")
		return
	}
	source := r.FormValue("source")
	if source != "kamera" && source != "galeri" {
		source = "galeri"
	}
	p := service.NewPhoto{ID: id, Stage: stage, UploaderID: viewer.UserID, Src: "/api/v1/photos/" + id, ObjectKey: &key,
		Sha256: hex.EncodeToString(sum[:]), Source: source, Bytes: domain.Ptr(int64(len(data))), Mime: &ctype}
	if v := r.FormValue("bookingId"); v != "" {
		p.BookingID = &v
	}
	if v := r.FormValue("angle"); v != "" {
		p.Angle = &v
	}
	if v, err := strconv.Atoi(r.FormValue("width")); err == nil && v > 0 {
		p.Width = &v
	}
	if v, err := strconv.Atoi(r.FormValue("height")); err == nil && v > 0 {
		p.Height = &v
	}
	rec, err := s.svc.AddPhoto(r.Context(), p)
	if err != nil {
		s.log.Error("photo insert", "err", err)
		writeErr(w, 500, "Gagal menyimpan data foto.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "photo": rec})
}

// demoPhoto registers a bundled sample image ("Pakai foto contoh") as evidence, hashed by the server.
func (s *Server) demoPhoto(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.viewerOr500(w, r)
	if !ok {
		return
	}
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
	if !service.PhotoStages[in.Stage] {
		writeErr(w, 400, "Tahap foto tidak valid.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.WebOrigin+in.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		if res != nil {
			res.Body.Close()
		}
		writeErr(w, 502, "Foto contoh tidak bisa diambil.")
		return
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 10<<20))
	sum := sha256.Sum256(data)
	rec, err := s.svc.AddPhoto(r.Context(), service.NewPhoto{BookingID: in.BookingID, Stage: in.Stage, UploaderID: viewer.UserID, Src: in.URL,
		Sha256: hex.EncodeToString(sum[:]), Source: "demo", Angle: in.Angle, Bytes: domain.Ptr(int64(len(data)))})
	if err != nil {
		s.log.Error("photo insert", "err", err)
		writeErr(w, 500, "Gagal menyimpan data foto.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "photo": rec})
}

// servePhoto serves an uploaded photo with its server-side hash. Like v1 it is not access-checked yet (an <img>
// cannot send a bearer header); signed URLs come with the media module (F3.3).
func (s *Server) servePhoto(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.Photo(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, "Gagal memuat foto.")
		return
	}
	if p == nil || p.ObjectKey == nil {
		http.NotFound(w, r)
		return
	}
	// Evidence never changes (append-only), so it can be cached forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-SHA256", p.Sha256)
	if !s.photos.Serve(w, r, *p.ObjectKey) {
		w.Header().Del("Cache-Control")
		w.Header().Del("X-Content-SHA256")
		http.NotFound(w, r)
	}
}
