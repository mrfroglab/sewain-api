package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image/jpeg"
	"testing"
)

func TestInspectAcceptsValidImages(t *testing.T) {
	cases := []struct {
		name, typ, mime, ext string
		data                 []byte
		w, h                 int
	}{
		{"jpeg", TypeJPEG, "image/jpeg", ".jpg", makeJPEG(t, 640, 480), 640, 480},
		{"png", TypePNG, "image/png", ".png", makePNG(t, 64, 33), 64, 33},
		{"webp", TypeWebP, "image/webp", ".webp", readWebP(t), 150, 100},
		{"jpeg zero padded", TypeJPEG, "image/jpeg", ".jpg", append(makeJPEG(t, 40, 40), 0, 0, 0, 0), 40, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info, err := Inspect(c.data)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			sum := sha256.Sum256(c.data)
			if info.Type != c.typ || info.MIME != c.mime || info.Ext != c.ext || info.Width != c.w || info.Height != c.h ||
				info.Bytes != int64(len(c.data)) || info.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatalf("info = %+v", info)
			}
		})
	}
}

func TestInspectHashMatchesBytes(t *testing.T) {
	data := makeJPEG(t, 100, 100)
	info, err := Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if info.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("hash differs from the received bytes")
	}
	// One flipped byte in the scan must change the hash (and still be judged on its own merits).
	data2 := append([]byte{}, data...)
	data2[len(data2)-10] ^= 0x01
	if info2, err := Inspect(data2); err == nil && info2.SHA256 == info.SHA256 {
		t.Fatal("hash did not change with the bytes")
	}
}

func TestInspectRejectsFakeAndMalicious(t *testing.T) {
	jpg := makeJPEG(t, 64, 64)
	pngData := makePNG(t, 64, 64)

	bigPNG := makePNG(t, 64, 64) // header claims 100000×100000: must fail before decoding pixels
	binary.BigEndian.PutUint32(bigPNG[16:], 100000)
	binary.BigEndian.PutUint32(bigPNG[20:], 100000)
	binary.BigEndian.PutUint32(bigPNG[29:], crc32.ChecksumIEEE(bigPNG[12:29]))

	webpTrailing := append(readWebP(t), []byte("<html><script>x</script>")...)

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, errEmpty},
		{"text named .jpg", []byte("ini bukan foto, cuma teks biasa yang diberi nama foto.jpg\n"), errUnsupported},
		{"svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(1)</script></svg>`), errUnsupported},
		{"html", []byte("<!doctype html><html><body><script>alert(1)</script></body></html>"), errUnsupported},
		{"gif", []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\xff\xff\xff\x00\x00\x00!\xf9\x04\x00\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"), errUnsupported},
		{"jpeg magic + html", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("<html><script>alert(1)</script></html>")...), errCorrupt},
		{"polyglot jpeg+zip", append(append([]byte{}, jpg...), []byte("PK\x03\x04\x14\x00\x00\x00evil.js")...), errTrailing},
		{"polyglot jpeg+html", append(append([]byte{}, jpg...), []byte("<html><script>alert(1)</script>")...), errTrailing},
		{"polyglot png+html", append(append([]byte{}, pngData...), []byte("<script>alert(1)</script>")...), errTrailing},
		{"polyglot webp+html", webpTrailing, errTrailing},
		{"jpeg comment script", insertJPEGSegment(jpg, 0xFE, []byte("<SCRIPT>alert(document.cookie)</SCRIPT>")), errMarkup},
		{"jpeg exif php", insertJPEGSegment(jpg, 0xE1, []byte("Exif\x00\x00<?php system($_GET['c']); ?>")), errMarkup},
		{"png text svg", insertPNGChunk(pngData, "tEXt", []byte("Comment\x00<svg onload=alert(1)>")), errMarkup},
		{"truncated jpeg", jpg[:len(jpg)/2], errCorrupt},
		{"truncated png", pngData[:len(pngData)-20], errCorrupt},
		{"over 10 MB", append(append([]byte{}, jpg...), make([]byte, MaxUploadBytes)...), errTooLarge},
		{"too small", makeJPEG(t, 16, 16), errTooSmall},
		{"too thin", makePNG(t, 400, 20), errTooSmall},
		{"too wide", makePNG(t, MaxDimension+1, 40), errTooWide},
		{"decompression bomb header", bigPNG, errTooWide},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Inspect(c.data)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			var ue *UploadError
			if !errors.As(err, &ue) || ue.Msg == "" || ue.Status < 400 {
				t.Fatalf("not a user-facing UploadError: %#v", err)
			}
		})
	}
}

func TestStripGPSRemovesLocation(t *testing.T) {
	base := makeJPEG(t, 120, 80)
	withGPS := insertJPEGSegment(base, 0xE1, exifWithGPS(6))
	xmp := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), []byte(`<x:xmpmeta><rdf:Description exif:GPSLatitude="7,46.2S"/></x:xmpmeta>`)...)
	withGPS = insertJPEGSegment(withGPS, 0xE1, xmp)
	if !bytes.Contains(withGPS, gpsRationals) {
		t.Fatal("fixture has no GPS")
	}
	if _, err := Inspect(withGPS); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	orig := append([]byte{}, withGPS...)

	out, err := StripGPS(withGPS)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(withGPS, orig) {
		t.Fatal("StripGPS modified its input")
	}
	if bytes.Contains(out, gpsRationals) || bytes.Contains(out, []byte("GPSLatitude")) || bytes.Contains(out, []byte{0x88, 0x25}) {
		t.Fatal("GPS data survived")
	}
	if jpegOrientation(out) != 6 {
		t.Fatalf("orientation not kept: %d", jpegOrientation(out))
	}
	if _, err := Inspect(out); err != nil {
		t.Fatalf("stripped file invalid: %v", err)
	}
	a, _ := jpeg.Decode(bytes.NewReader(orig))
	b, _ := jpeg.Decode(bytes.NewReader(out))
	if a.Bounds() != b.Bounds() || a.At(10, 10) != b.At(10, 10) || a.At(100, 70) != b.At(100, 70) {
		t.Fatal("pixels changed")
	}

	// Orientation 1 (normal) leaves no EXIF at all.
	out1, err := StripGPS(insertJPEGSegment(base, 0xE1, exifWithGPS(1)))
	if err != nil || bytes.Contains(out1, []byte("Exif\x00\x00")) {
		t.Fatalf("expected no EXIF left, err=%v", err)
	}
}

func TestStripGPSPNGAndWebP(t *testing.T) {
	p := insertPNGChunk(makePNG(t, 40, 40), "eXIf", exifWithGPS(1)[6:])
	out, err := StripGPS(p)
	if err != nil || bytes.Contains(out, gpsRationals) {
		t.Fatalf("png: err=%v", err)
	}
	if _, err := Inspect(out); err != nil {
		t.Fatalf("png stripped invalid: %v", err)
	}

	w := readWebP(t)
	exif := exifWithGPS(1)[6:]
	chunk := []byte("EXIF\x00\x00\x00\x00")
	binary.LittleEndian.PutUint32(chunk[4:], uint32(len(exif)))
	chunk = append(chunk, exif...)
	if len(exif)%2 == 1 {
		chunk = append(chunk, 0)
	}
	ww := append(append([]byte{}, w...), chunk...)
	binary.LittleEndian.PutUint32(ww[4:], uint32(len(ww)-8))
	if !bytes.Contains(ww, gpsRationals) {
		t.Fatal("fixture")
	}
	out, err = StripGPS(ww)
	if err != nil || bytes.Contains(out, gpsRationals) {
		t.Fatalf("webp: err=%v", err)
	}
	if !bytes.Equal(out, w) {
		t.Fatal("webp: expected the original bytes back")
	}
	if _, err := StripGPS([]byte("not an image")); err == nil {
		t.Fatal("expected error for non-image")
	}
}
