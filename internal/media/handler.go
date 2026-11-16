package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PhotoLookup resolves photo metadata for the handler. The core implements it on top of the `photos` and
// `bookings` tables. It returns ErrNotFound for unknown IDs; booking is nil for listing photos.
type PhotoLookup interface {
	LookupPhoto(ctx context.Context, id string) (photo Photo, booking *Booking, err error)
}

// PhotoLookupFunc adapts a function to PhotoLookup.
type PhotoLookupFunc func(ctx context.Context, id string) (Photo, *Booking, error)

func (f PhotoLookupFunc) LookupPhoto(ctx context.Context, id string) (Photo, *Booking, error) {
	return f(ctx, id)
}

// Handler serves GET/HEAD /api/v1/photos/{id}.
//
// Access: public (referensi) photos need nothing. Evidence needs either a valid signature (the capability
// minted by SignURL/SignFor after a CanView check) or, if SessionViewer is set, a session whose viewer
// passes CanView. A signature for v=wm only ever yields the watermarked copy.
type Handler struct {
	Storage Storage
	Signer  *Signer
	Lookup  PhotoLookup
	// SessionViewer optionally resolves the logged-in viewer (cookie session). nil = signatures only.
	SessionViewer func(*http.Request) *Viewer
	Log           *slog.Logger
	Now           func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeMediaErr(w, http.StatusMethodNotAllowed, "Metode tidak didukung.")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		id = strings.TrimPrefix(r.URL.Path, photoPathPrefix)
	}
	if id == "" || strings.Contains(id, "/") {
		writeMediaErr(w, http.StatusNotFound, "Foto tidak ditemukan.")
		return
	}
	ctx := r.Context()
	photo, booking, err := h.Lookup.LookupPhoto(ctx, id)
	if errors.Is(err, ErrNotFound) || (err == nil && !IsPublicStage(photo.Stage) && !IsEvidenceStage(photo.Stage)) {
		// KYC and unknown stages are indistinguishable from a missing photo.
		writeMediaErr(w, http.StatusNotFound, "Foto tidak ditemukan.")
		return
	}
	if err != nil {
		h.log().Error("photo lookup", "id", id, "err", err)
		writeMediaErr(w, http.StatusInternalServerError, "Foto tidak bisa dimuat.")
		return
	}

	public := IsPublicStage(photo.Stage)
	variant := ""
	cacheControl := "public, max-age=31536000, immutable"
	if !public {
		claims, sigErr := h.verify(id, r)
		switch {
		case sigErr == nil:
			variant = claims.Variant
			maxAge := max(int(claims.Expires.Sub(h.now())/time.Second), 0)
			cacheControl = "private, max-age=" + strconv.Itoa(maxAge)
		case h.SessionViewer != nil && sigErr == ErrSignatureMissing && CanView(h.SessionViewer(r), photo, booking):
			if v := h.SessionViewer(r); !v.Admin {
				variant = VariantWatermark
			}
			cacheControl = "private, no-cache"
		case sigErr == ErrSignatureExpired:
			writeMediaErr(w, http.StatusForbidden, "Tautan foto sudah kedaluwarsa. Muat ulang halaman, ya.")
			return
		default:
			writeMediaErr(w, http.StatusForbidden, "Kamu tidak punya akses ke foto ini.")
			return
		}
	}

	orig, _, err := h.Storage.Get(ctx, photo.ObjectKey)
	if errors.Is(err, ErrNotFound) {
		writeMediaErr(w, http.StatusNotFound, "Foto tidak ditemukan.")
		return
	}
	if err != nil {
		h.log().Error("photo get", "id", id, "err", err)
		writeMediaErr(w, http.StatusInternalServerError, "Foto tidak bisa dimuat.")
		return
	}
	// Evidence integrity: the stored bytes must still hash to what was recorded at upload (FR-P3).
	origSum := sha256.Sum256(orig)
	origHex := hex.EncodeToString(origSum[:])
	if photo.SHA256 != "" && !strings.EqualFold(photo.SHA256, origHex) {
		h.log().Error("photo hash mismatch", "id", id, "want", photo.SHA256, "got", origHex)
		writeMediaErr(w, http.StatusInternalServerError, "Foto tidak lolos cek integritas.")
		return
	}

	body, bodyHex := orig, origHex
	if variant == VariantWatermark {
		wm, err := WatermarkedCopy(ctx, h.Storage, photo, booking)
		if err != nil {
			h.log().Error("photo watermark", "id", id, "err", err)
			writeMediaErr(w, http.StatusInternalServerError, "Foto tidak bisa dimuat.")
			return
		}
		sum := sha256.Sum256(wm)
		body, bodyHex = wm, hex.EncodeToString(sum[:])
	}

	ctype := MIMEType(DetectType(body))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	hd := w.Header()
	hd.Set("Content-Type", ctype)
	hd.Set("Cache-Control", cacheControl)
	hd.Set("ETag", `"`+bodyHex+`"`)
	hd.Set("X-Content-SHA256", bodyHex)
	hd.Set("X-Original-SHA256", origHex)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hd.Set("Referrer-Policy", "no-referrer")
	http.ServeContent(w, r, "", photo.ServerTime, bytes.NewReader(body))
}

func (h *Handler) verify(id string, r *http.Request) (Claims, error) {
	q := r.URL.Query()
	if q.Get("sig") == "" {
		return Claims{}, ErrSignatureMissing
	}
	if h.Signer == nil {
		return Claims{}, ErrSignatureInvalid
	}
	return h.Signer.Verify(id, q)
}

func writeMediaErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}
