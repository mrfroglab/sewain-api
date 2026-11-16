package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/jpeg"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	h        *Handler
	signer   *Signer
	st       *LocalStorage
	now      time.Time
	evidence Photo
	listing  Photo
	booking  *Booking
	origJPEG []byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 11, 16, 7, 5, 0, 0, time.UTC)}
	st, err := NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = st
	f.signer = newTestSigner(t, &f.now)
	ctx := context.Background()

	f.origJPEG = makeJPEG(t, 320, 240)
	info, err := Inspect(f.origJPEG)
	if err != nil {
		t.Fatal(err)
	}
	f.booking = &Booking{ID: "b-1", Code: "BK-7Q2X", RenterID: "u-renter", ProviderID: "u-provider"}
	f.evidence = Photo{ID: "p-ev", Stage: StageCheckout, BookingID: "b-1", UploaderID: "u-renter",
		ObjectKey: PhotoKey("p-ev", info.Ext), SHA256: info.SHA256, MIME: info.MIME, ServerTime: f.now}
	if err := st.Put(ctx, f.evidence.ObjectKey, f.origJPEG, info.MIME); err != nil {
		t.Fatal(err)
	}
	pngData := makePNG(t, 64, 64)
	pinfo, _ := Inspect(pngData)
	f.listing = Photo{ID: "p-ref", Stage: StageReferensi, ListingID: "l-1", ObjectKey: PhotoKey("p-ref", pinfo.Ext), SHA256: pinfo.SHA256, ServerTime: f.now}
	if err := st.Put(ctx, f.listing.ObjectKey, pngData, pinfo.MIME); err != nil {
		t.Fatal(err)
	}
	kyc := Photo{ID: "p-ktp", Stage: "ktp", ObjectKey: PhotoKey("p-ktp", ".jpg")}
	_ = st.Put(ctx, kyc.ObjectKey, f.origJPEG, "image/jpeg")

	photos := map[string]Photo{f.evidence.ID: f.evidence, f.listing.ID: f.listing, kyc.ID: kyc}
	f.h = &Handler{
		Storage: st, Signer: f.signer, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return f.now },
		Lookup: PhotoLookupFunc(func(_ context.Context, id string) (Photo, *Booking, error) {
			p, ok := photos[id]
			if !ok {
				return Photo{}, nil, ErrNotFound
			}
			if p.BookingID != "" {
				return p, f.booking, nil
			}
			return p, nil, nil
		}),
	}
	return f
}

func (f *fixture) get(t *testing.T, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/photos/{id}", f.h)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func errMsg(rec *httptest.ResponseRecorder) string {
	var body struct{ Error string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error
}

func TestHandlerEvidenceAccess(t *testing.T) {
	f := newFixture(t)

	t.Run("no signature", func(t *testing.T) {
		rec := f.get(t, "/api/v1/photos/p-ev")
		if rec.Code != 403 || errMsg(rec) == "" {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	t.Run("signed original (admin)", func(t *testing.T) {
		rec := f.get(t, f.signer.SignFor(&Viewer{ID: "u-admin", Admin: true}, f.evidence, f.booking, 0))
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), f.origJPEG) {
			t.Fatalf("code=%d", rec.Code)
		}
		h := rec.Header()
		if h.Get("Content-Type") != "image/jpeg" || h.Get("X-Content-SHA256") != f.evidence.SHA256 ||
			h.Get("ETag") != `"`+f.evidence.SHA256+`"` || !strings.HasPrefix(h.Get("Cache-Control"), "private, max-age=") ||
			h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("headers = %v", h)
		}
		// Conditional refetch.
		rec2 := f.get(t, f.signer.SignFor(&Viewer{ID: "u-admin", Admin: true}, f.evidence, f.booking, 0), "If-None-Match", h.Get("ETag"))
		if rec2.Code != 304 {
			t.Fatalf("If-None-Match: code=%d", rec2.Code)
		}
	})
	t.Run("signed watermarked (party)", func(t *testing.T) {
		rec := f.get(t, f.signer.SignFor(&Viewer{ID: "u-renter"}, f.evidence, f.booking, 0))
		if rec.Code != 200 || bytes.Equal(rec.Body.Bytes(), f.origJPEG) {
			t.Fatalf("code=%d, expected watermarked copy", rec.Code)
		}
		if _, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes())); err != nil {
			t.Fatalf("watermarked body is not a JPEG: %v", err)
		}
		sum := sha256.Sum256(rec.Body.Bytes())
		if rec.Header().Get("X-Content-SHA256") != hex.EncodeToString(sum[:]) || rec.Header().Get("X-Original-SHA256") != f.evidence.SHA256 {
			t.Fatalf("hash headers = %v", rec.Header())
		}
		if ok, _ := f.st.Exists(context.Background(), WatermarkKey("p-ev")); !ok {
			t.Fatal("watermark not cached")
		}
		stored, _, _ := f.st.Get(context.Background(), f.evidence.ObjectKey)
		if !bytes.Equal(stored, f.origJPEG) {
			t.Fatal("original changed")
		}
	})
	t.Run("signature for another photo", func(t *testing.T) {
		u := f.signer.SignURL("p-other", "u-renter", 0)
		u = strings.Replace(u, "p-other", "p-ev", 1)
		if rec := f.get(t, u); rec.Code != 403 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	t.Run("expired", func(t *testing.T) {
		u := f.signer.SignFor(&Viewer{ID: "u-renter"}, f.evidence, f.booking, time.Minute)
		saved := f.now
		f.now = f.now.Add(2 * time.Hour)
		defer func() { f.now = saved }()
		rec := f.get(t, u)
		if rec.Code != 403 || !strings.Contains(errMsg(rec), "kedaluwarsa") {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	t.Run("session viewer", func(t *testing.T) {
		var viewer *Viewer
		f.h.SessionViewer = func(*http.Request) *Viewer { return viewer }
		defer func() { f.h.SessionViewer = nil }()
		viewer = &Viewer{ID: "u-stranger"}
		if rec := f.get(t, "/api/v1/photos/p-ev"); rec.Code != 403 {
			t.Fatalf("stranger session: code=%d", rec.Code)
		}
		viewer = &Viewer{ID: "u-provider"}
		rec := f.get(t, "/api/v1/photos/p-ev")
		if rec.Code != 200 || bytes.Equal(rec.Body.Bytes(), f.origJPEG) || rec.Header().Get("Cache-Control") != "private, no-cache" {
			t.Fatalf("party session: code=%d", rec.Code)
		}
	})
	t.Run("tampered stored file", func(t *testing.T) {
		g := newFixture(t)
		g.evidence.SHA256 = strings.Repeat("0", 64)
		g.h.Lookup = PhotoLookupFunc(func(context.Context, string) (Photo, *Booking, error) { return g.evidence, g.booking, nil })
		rec := g.get(t, g.signer.SignFor(&Viewer{ID: "a", Admin: true}, g.evidence, g.booking, 0))
		if rec.Code != 500 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
}

func TestHandlerPublicAndHidden(t *testing.T) {
	f := newFixture(t)
	rec := f.get(t, "/api/v1/photos/p-ref")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(rec.Header().Get("Cache-Control"), "public") {
		t.Fatalf("listing photo: code=%d headers=%v", rec.Code, rec.Header())
	}
	// KYC never goes through this path, signed or not.
	if rec := f.get(t, f.signer.SignURL("p-ktp", "u-admin", 0)); rec.Code != 404 {
		t.Fatalf("kyc: code=%d", rec.Code)
	}
	if rec := f.get(t, "/api/v1/photos/p-missing"); rec.Code != 404 {
		t.Fatalf("missing: code=%d", rec.Code)
	}
	f.h.Lookup = PhotoLookupFunc(func(context.Context, string) (Photo, *Booking, error) {
		return Photo{}, nil, errors.New("db down")
	})
	if rec := f.get(t, "/api/v1/photos/p-ref"); rec.Code != 500 {
		t.Fatalf("lookup error: code=%d", rec.Code)
	}
}
