package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// WIB is Asia/Jakarta without depending on tzdata being installed in the container.
var WIB = time.FixedZone("WIB", 7*3600)

const watermarkJPEGQuality = 85

// WatermarkText is the label stamped on evidence for display (FR-P5): booking code (or ID) and the server
// upload time in WIB, e.g. "BK-7Q2X | 16-11-2026 14:05 WIB".
func WatermarkText(photo Photo, booking *Booking) string {
	id := photo.BookingID
	if booking != nil {
		if booking.Code != "" {
			id = booking.Code
		} else if booking.ID != "" {
			id = booking.ID
		}
	}
	ts := photo.ServerTime
	if ts.IsZero() {
		ts = time.Now()
	}
	label := ts.In(WIB).Format("02-01-2006 15:04") + " WIB"
	if id == "" {
		return label
	}
	return id + " | " + label
}

// Watermark decodes a JPEG/PNG/WebP image, applies the EXIF orientation, stamps text in the bottom-right
// corner on a semi-transparent band and returns a new JPEG. src is never modified.
func Watermark(src []byte, text string) ([]byte, error) {
	typ := DetectType(src)
	if typ == "" {
		return nil, errUnsupported
	}
	img, err := decodeImage(typ, src)
	if err != nil {
		return nil, errCorrupt
	}
	orientation := 1
	if typ == TypeJPEG {
		orientation = jpegOrientation(src)
	}
	canvas := orient(img, orientation)
	stamp(canvas, text)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: watermarkJPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jpegOrientation(data []byte) int {
	segs, _, err := parseJPEG(data)
	if err != nil {
		return 1
	}
	for _, sg := range segs {
		body := data[sg.start:sg.end]
		if sg.marker == 0xE1 && len(body) > 4 && bytes.HasPrefix(body[4:], exifHeader) {
			if o := exifOrientation(body[4+len(exifHeader):]); o >= 1 && o <= 8 {
				return o
			}
		}
	}
	return 1
}

// orient copies img into a fresh RGBA, transformed per EXIF orientation 1–8.
func orient(img image.Image, o int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if o < 1 || o > 8 {
		o = 1
	}
	if o == 1 {
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
		return dst
	}
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		for x := range w {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// stamp draws text bottom-right. The bitmap font is rendered small and scaled so the label is ~3% of the
// shorter side tall, readable on phone photos without hiding the item.
func stamp(dst *image.RGBA, text string) {
	face := basicfont.Face7x13
	d := &font.Drawer{Face: face}
	tw := d.MeasureString(text).Ceil()
	const pad = 3
	lw, lh := tw+2*pad, face.Height+2*pad
	label := image.NewRGBA(image.Rect(0, 0, lw, lh))
	draw.Draw(label, label.Bounds(), image.NewUniform(color.NRGBA{0, 0, 0, 110}), image.Point{}, draw.Src)
	d.Dst = label
	d.Src = image.NewUniform(color.NRGBA{255, 255, 255, 215})
	d.Dot = fixed.P(pad, pad+face.Ascent)
	d.DrawString(text)

	b := dst.Bounds()
	short := min(b.Dx(), b.Dy())
	scale := max(float64(short)*0.03/float64(lh), 1)
	sw, sh := int(float64(lw)*scale), int(float64(lh)*scale)
	if sw > b.Dx()-4 { // very narrow image: shrink to fit
		sh = sh * (b.Dx() - 4) / sw
		sw = b.Dx() - 4
	}
	margin := max(short/60, 2)
	r := image.Rect(b.Max.X-sw-margin, b.Max.Y-sh-margin, b.Max.X-margin, b.Max.Y-margin)
	xdraw.CatmullRom.Scale(dst, r, label, label.Bounds(), xdraw.Over, nil)
}

// WatermarkedCopy returns the watermarked display copy of photo, rendering it once and caching it under
// WatermarkKey(photo.ID). The original object is only read.
func WatermarkedCopy(ctx context.Context, st Storage, photo Photo, booking *Booking) ([]byte, error) {
	key := WatermarkKey(photo.ID)
	if data, _, err := st.Get(ctx, key); err == nil {
		return data, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	orig, _, err := st.Get(ctx, photo.ObjectKey)
	if err != nil {
		return nil, err
	}
	out, err := Watermark(orig, WatermarkText(photo, booking))
	if err != nil {
		return nil, err
	}
	// A concurrent request may have cached it first; both renders are equivalent.
	if err := st.Put(ctx, key, out, "image/jpeg"); err != nil && !errors.Is(err, ErrExists) {
		return nil, err
	}
	return out, nil
}
