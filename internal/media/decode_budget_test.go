package media

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bombPNG builds a w×h PNG (colour type 6 = RGBA, bitDepth 8 or 16) whose pixels are all zero. Written
// row by row through zlib, so a 64-megapixel file is only a few hundred KB and needs no image in memory.
func bombPNG(t testing.TB, w, h int, bitDepth byte) []byte {
	t.Helper()
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = bitDepth, 6
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
	row := make([]byte, 1+w*4*int(bitDepth/8))
	for range h {
		if _, err := zw.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, pngChunkBytes("IHDR", ihdr)...)
	out = append(out, pngChunkBytes("IDAT", z.Bytes())...)
	return append(out, pngChunkBytes("IEND", nil)...)
}

// Finding M1: an 8000×8000 RGBA PNG (~250 KB on the wire) passed the dimension check and was fully decoded
// into a 256 MB NRGBA buffer (512 MB at 16 bit). The API container has mem_limit 256m, so one upload
// OOM-killed it. Inspect must refuse it from the header, before allocating pixels.
func TestInspectRejectsDecompressionBombBeforeDecoding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		w, h     int
		bitDepth byte
	}{
		{"8000x8000 rgba8", 8000, 8000, 8},
		{"5000x5000 rgba8", 5000, 5000, 8},
		{"3000x3000 rgba16", 3000, 3000, 16}, // 9 MP × 8 B = 72 MB decoded
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bombPNG(t, tc.w, tc.h, tc.bitDepth)
			if len(data) > MaxUploadBytes {
				t.Fatalf("test bomb is %d bytes, larger than the upload limit", len(data))
			}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := Inspect(data)
			runtime.ReadMemStats(&after)
			var ue *UploadError
			if !errors.As(err, &ue) || ue != errTooManyPixels {
				t.Fatalf("Inspect err = %v, want errTooManyPixels", err)
			}
			if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
				t.Fatalf("Inspect allocated %d MB before rejecting", alloc>>20)
			}
		})
	}
}

func TestInspectAcceptsLargestAllowedPhoto(t *testing.T) {
	// 4096×4096 RGBA8 = 64 MB decoded: exactly on the budget.
	if _, err := Inspect(bombPNG(t, 4096, 4096, 8)); err != nil {
		t.Fatalf("Inspect(4096x4096) = %v", err)
	}
}

func TestWatermarkRefusesOverBudgetOriginal(t *testing.T) {
	// Originals stored before the budget existed must not be decoded by the watermark path either.
	if _, err := Watermark(bombPNG(t, 5000, 5000, 8), "x"); !errors.Is(err, errTooManyPixels) {
		t.Fatalf("Watermark err = %v, want errTooManyPixels", err)
	}
}

// Full decodes run one at a time, so N concurrent uploads or watermark renders cannot each hold a
// 64 MB buffer at once.
func TestDecodesAreSerialized(t *testing.T) {
	img := makePNG(t, 600, 400)
	var cur, peak atomic.Int32
	decodeHook = func() func() {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		return func() { cur.Add(-1) }
	}
	defer func() { decodeHook = nil }()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if i%2 == 0 {
				if _, err := Inspect(img); err != nil {
					t.Error(err)
				}
			} else if _, err := Watermark(img, "BK-1"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if p := peak.Load(); p != MaxConcurrentDecodes {
		t.Fatalf("peak concurrent decodes = %d, want %d", p, MaxConcurrentDecodes)
	}
}
