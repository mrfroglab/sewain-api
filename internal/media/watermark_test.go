package media

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"testing"
	"time"
)

// regionDiff is the mean absolute difference per channel (0–255) between a and b inside r.
func regionDiff(a, b image.Image, r image.Rectangle) float64 {
	var sum, n float64
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			r1, g1, b1, _ := a.At(x, y).RGBA()
			r2, g2, b2, _ := b.At(x, y).RGBA()
			for _, d := range []int{int(r1>>8) - int(r2>>8), int(g1>>8) - int(g2>>8), int(b1>>8) - int(b2>>8)} {
				sum += float64(max(d, -d))
				n++
			}
		}
	}
	return sum / n
}

func TestWatermarkVisual(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
	}{
		{"jpeg", makeJPEG(t, 800, 600)},
		{"png", makePNG(t, 400, 300)},
		{"webp", readWebP(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := append([]byte{}, tc.src...)
			out, err := Watermark(tc.src, "BK-7Q2X | 16-11-2026 14:05 WIB")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(tc.src, orig) {
				t.Fatal("source bytes modified")
			}
			if DetectType(out) != TypeJPEG {
				t.Fatal("output is not JPEG")
			}
			got, err := jpeg.Decode(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("output does not decode: %v", err)
			}
			src, _ := decodeImage(DetectType(tc.src), tc.src)
			b := src.Bounds()
			if got.Bounds().Size() != b.Size() {
				t.Fatalf("size %v → %v", b.Size(), got.Bounds().Size())
			}
			w, h := b.Dx(), b.Dy()
			corner := image.Rect(w*3/4, h*9/10, w-2, h-2)
			top := image.Rect(2, 2, w/3, h/3)
			dc, dt := regionDiff(src, got, corner), regionDiff(src, got, top)
			if dc < 8 {
				t.Fatalf("no visible stamp in bottom-right corner (diff %.1f)", dc)
			}
			if dt > 4 {
				t.Fatalf("image changed away from the stamp (diff %.1f)", dt)
			}
		})
	}
	if _, err := Watermark([]byte("bukan gambar"), "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestWatermarkAppliesOrientation(t *testing.T) {
	src := insertJPEGSegment(makeJPEG(t, 200, 100), 0xE1, exifWithGPS(6)) // rotate 90° CW
	out, err := Watermark(src, "x")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 100 || cfg.Height != 200 {
		t.Fatalf("orientation not applied: %dx%d", cfg.Width, cfg.Height)
	}
	if bytes.Contains(out, gpsRationals) {
		t.Fatal("display copy carries GPS")
	}
}

func TestWatermarkText(t *testing.T) {
	ts := time.Date(2026, 11, 16, 7, 5, 0, 0, time.UTC) // 14:05 WIB
	p := Photo{BookingID: "b-1", ServerTime: ts}
	if got := WatermarkText(p, &Booking{ID: "b-1", Code: "BK-7Q2X"}); got != "BK-7Q2X | 16-11-2026 14:05 WIB" {
		t.Fatalf("got %q", got)
	}
	if got := WatermarkText(p, nil); got != "b-1 | 16-11-2026 14:05 WIB" {
		t.Fatalf("got %q", got)
	}
}

func TestWatermarkedCopyCaches(t *testing.T) {
	st, _ := NewLocalStorage(t.TempDir())
	defer st.Close()
	ctx := context.Background()
	src := makeJPEG(t, 300, 200)
	p := Photo{ID: "p-1", ObjectKey: PhotoKey("p-1", ".jpg"), BookingID: "b-1", ServerTime: time.Now()}
	if err := st.Put(ctx, p.ObjectKey, src, "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	a, err := WatermarkedCopy(ctx, st, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	cached, ct, err := st.Get(ctx, WatermarkKey("p-1"))
	if err != nil || ct != "image/jpeg" || !bytes.Equal(cached, a) {
		t.Fatalf("cache: %v %q", err, ct)
	}
	// Second call is served from the cache even if the label would differ now.
	p.ServerTime = p.ServerTime.Add(48 * time.Hour)
	b, _ := WatermarkedCopy(ctx, st, p, nil)
	if !bytes.Equal(a, b) {
		t.Fatal("not served from cache")
	}
	orig, _, _ := st.Get(ctx, p.ObjectKey)
	if !bytes.Equal(orig, src) {
		t.Fatal("original changed")
	}
}
