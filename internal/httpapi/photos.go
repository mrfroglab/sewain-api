package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mrfroglab/sewain-api/internal/domain"
	"github.com/mrfroglab/sewain-api/internal/media"
	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
	"github.com/mrfroglab/sewain-api/internal/service"
)

// signWindow is how long a signed photo URL stays the same (media rounds expiry up to 15 minutes). The snapshot
// ETag includes the window so a cached snapshot never hands out URLs that have expired.
const signWindow = 15 * time.Minute

// state returns the viewer's snapshot (ROADMAP §4) with an ETag; If-None-Match → 304. Photo src values are signed
// for this viewer (media.SignFor): public listing photos stay plain, evidence gets a short-lived signed URL
// (watermarked for the parties, original for admins), and evidence the viewer may not see is left out.
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	viewer := viewerOf(r)
	window := time.Now().Unix() / int64(signWindow/time.Second)
	etag, _, err := s.svc.ETag(r.Context(), viewer)
	if err != nil {
		s.log.Error("etag", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal memuat data.")
		return
	}
	etag = windowed(etag, window)
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
		writeErr(w, http.StatusInternalServerError, "Gagal memuat data.")
		return
	}
	s.signPhotos(snap, viewer)
	b, err := service.SnapshotJSON(snap)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Gagal memuat data.")
		return
	}
	// The ETag describes the version the snapshot was built from (it may be newer than the one checked above),
	// so a 304 never hides a newer state.
	w.Header().Set("ETag", windowed(s.svc.ETagFor(viewer, snap.Version, snap.ClockOffset), window))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b) // #nosec G705 -- JSON (Content-Type application/json, nosniff), not HTML
}

func windowed(etag string, window int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", etag, window)))
	return `W/"` + hex.EncodeToString(sum[:12]) + `"`
}

// signPhotos replaces the src of every stored photo with the URL this viewer may use. Bundled demo images (/img/…)
// keep their static URL. With SNAPSHOT_SCOPE=full (demo fallback, everything visible like v1) evidence the viewer is
// not party to is signed as the renter's watermarked copy.
func (s *Server) signPhotos(snap *service.Snapshot, viewer *service.Viewer) {
	bookings := make(map[string]*media.Booking, len(snap.Bookings))
	for _, b := range snap.Bookings {
		bookings[b.ID] = &media.Booking{ID: b.ID, Code: b.Code, RenterID: b.RenterID, ProviderID: b.ProviderID, Status: b.Status}
	}
	full := snap.Scope == service.ScopeFull
	mv := mediaViewer(viewer)
	kept := snap.Photos[:0]
	for _, p := range snap.Photos {
		if !strings.HasPrefix(p.Src, photoPath) {
			kept = append(kept, p)
			continue
		}
		mp := media.Photo{ID: p.ID, Stage: p.Stage, BookingID: domain.Deref(p.BookingID, ""), UploaderID: p.UploaderID}
		b := bookings[mp.BookingID]
		src := s.deps.Signer.SignFor(mv, mp, b, 0)
		if src == "" && full && b != nil {
			src = s.deps.Signer.SignFor(&media.Viewer{ID: b.RenterID}, mp, b, 0)
		}
		if src == "" {
			continue
		}
		p.Src = src
		kept = append(kept, p)
	}
	snap.Photos = kept
}

// uploadPhoto stores evidence or a listing photo: validated by media.Inspect (type from the bytes, size, dimensions,
// no hidden payload), GPS metadata stripped from public listing photos, SHA-256 of the stored bytes and the server
// timestamp recorded (FR-P2/P3, FR-P7). Responds with the photo and a URL signed for the uploader.
func (s *Server) uploadPhoto(w http.ResponseWriter, r *http.Request) {
	viewer := viewerOf(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "Masuk dulu untuk mengunggah foto.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, middleware.UploadBodyLimit)
	if err := r.ParseMultipartForm(1 << 20); err != nil { // #nosec G120 -- body capped by MaxBytesReader above (12 MB), 1 MB in memory
		if middleware.IsBodyTooLarge(err) {
			writeErr(w, http.StatusRequestEntityTooLarge, "Foto terlalu besar (maks 10 MB).")
			return
		}
		writeErr(w, http.StatusBadRequest, "Unggahan tidak valid. Kirim ulang fotonya.")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	stage := r.FormValue("stage")
	if !service.PhotoStages[stage] {
		writeErr(w, http.StatusBadRequest, "Tahap foto tidak valid.")
		return
	}
	var bookingID *string
	if v := r.FormValue("bookingId"); v != "" {
		bookingID = &v
		if ok, err := s.svc.CanUploadTo(r.Context(), v, viewer); err != nil {
			s.log.Error("photo booking check", "err", err)
			writeErr(w, http.StatusInternalServerError, "Gagal menyimpan foto.")
			return
		} else if !ok {
			writeErr(w, http.StatusForbidden, "Booking tidak ditemukan.")
			return
		}
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "File foto wajib.")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, media.MaxUploadBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Gagal membaca foto.")
		return
	}
	info, err := media.Inspect(data)
	if stage == media.StageReferensi && err == nil {
		// Public listing photo: drop EXIF/XMP (GPS). Evidence stays byte-identical so its hash proves the upload.
		if stripped, serr := media.StripGPS(data); serr == nil {
			data = stripped
			info, err = media.Inspect(data)
		}
	}
	var ue *media.UploadError
	if errors.As(err, &ue) {
		writeErr(w, ue.Status, ue.Msg)
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Foto tidak valid.")
		return
	}
	id := domain.NewID("p")
	key := media.PhotoKey(id, info.Ext)
	if err := s.deps.Storage.Put(r.Context(), key, data, info.MIME); err != nil {
		s.log.Error("photo store", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal menyimpan foto.")
		return
	}
	source := r.FormValue("source")
	if source != "kamera" && source != "galeri" {
		source = "galeri"
	}
	p := service.NewPhoto{ID: id, BookingID: bookingID, Stage: stage, UploaderID: viewer.UserID, Src: photoPath + id, ObjectKey: &key,
		Sha256: info.SHA256, Source: source, Bytes: domain.Ptr(info.Bytes), Mime: domain.Ptr(info.MIME),
		Width: domain.Ptr(info.Width), Height: domain.Ptr(info.Height)}
	if v := r.FormValue("angle"); v != "" {
		p.Angle = &v
	}
	rec, err := s.svc.AddPhoto(r.Context(), p)
	if err != nil {
		s.log.Error("photo insert", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal menyimpan data foto.")
		return
	}
	rec.Src = s.signedSrc(r.Context(), viewer, id, rec.Src)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "photo": rec})
}

// signedSrc signs a stored photo for the viewer (falls back to the canonical path).
func (s *Server) signedSrc(ctx context.Context, viewer *service.Viewer, id, fallback string) string {
	mp, mb, err := s.svc.LookupPhoto(ctx, id)
	if err != nil {
		return fallback
	}
	if src := s.deps.Signer.SignFor(mediaViewer(viewer), mp, mb, 0); src != "" {
		return src
	}
	return fallback
}

// demoPhoto registers a bundled sample image ("Pakai foto contoh") as evidence, hashed by the server (DEMO_MODE only).
// The photo keeps its static /img/… URL (no stored object).
func (s *Server) demoPhoto(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.DemoMode {
		// Outside the demo, evidence must be a real upload (FR-P2): no stock pictures with a server timestamp.
		writeErr(w, http.StatusForbidden, "Foto contoh hanya tersedia di DEMO_MODE.")
		return
	}
	viewer := viewerOf(r)
	if viewer == nil {
		writeErr(w, http.StatusUnauthorized, "Masuk dulu.")
		return
	}
	var in struct {
		URL       string  `json:"url"`
		Stage     string  `json:"stage"`
		BookingID *string `json:"bookingId"`
		Angle     *string `json:"angle"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil || !strings.HasPrefix(in.URL, "/img/") || strings.Contains(in.URL, "..") {
		writeErr(w, http.StatusBadRequest, "URL foto contoh tidak valid.")
		return
	}
	if !service.PhotoStages[in.Stage] {
		writeErr(w, http.StatusBadRequest, "Tahap foto tidak valid.")
		return
	}
	if in.BookingID != nil {
		if ok, err := s.svc.CanUploadTo(r.Context(), *in.BookingID, viewer); err != nil || !ok {
			writeErr(w, http.StatusForbidden, "Booking tidak ditemukan.")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.WebOrigin+in.URL, nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "URL foto contoh tidak valid.")
		return
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "Foto contoh tidak bisa diambil.")
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, "Foto contoh tidak bisa diambil.")
		return
	}
	data, _ := io.ReadAll(io.LimitReader(res.Body, media.MaxUploadBytes))
	sum := sha256.Sum256(data)
	rec, err := s.svc.AddPhoto(r.Context(), service.NewPhoto{BookingID: in.BookingID, Stage: in.Stage, UploaderID: viewer.UserID, Src: in.URL,
		Sha256: hex.EncodeToString(sum[:]), Source: "demo", Angle: in.Angle, Bytes: domain.Ptr(int64(len(data)))})
	if err != nil {
		s.log.Error("photo insert", "err", err)
		writeErr(w, http.StatusInternalServerError, "Gagal menyimpan data foto.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "photo": rec})
}
