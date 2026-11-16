package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"testing"
)

// gradient is a deterministic test picture with some detail (not a flat color).
func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x * 255 / max(w-1, 1)), uint8(y * 255 / max(h-1, 1)), 90, 255})
		}
	}
	return img
}

func makeJPEG(t testing.TB, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gradient(w, h), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makePNG(t testing.TB, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, gradient(w, h)); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readWebP(t testing.TB) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/sample.webp")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// insertJPEGSegment puts a marker segment right after SOI.
func insertJPEGSegment(jpg []byte, marker byte, payload []byte) []byte {
	seg := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	seg = append(seg, payload...)
	out := append([]byte{}, jpg[:2]...)
	out = append(out, seg...)
	return append(out, jpg[2:]...)
}

// gpsRationals is the GPSLatitude value written by exifWithGPS: 7° 46' 12.3456" (S), near UGM.
var gpsRationals = []byte{
	0, 0, 0, 7, 0, 0, 0, 1,
	0, 0, 0, 46, 0, 0, 0, 1,
	0, 1, 0xE2, 0x40, 0, 0, 0x27, 0x10,
}

// exifWithGPS builds an APP1 EXIF payload: IFD0 {Orientation=orientation, GPSInfo→GPS IFD},
// GPS IFD {GPSLatitudeRef="S", GPSLatitude=gpsRationals}.
func exifWithGPS(orientation uint16) []byte {
	be := binary.BigEndian
	t := []byte{'M', 'M', 0, 42, 0, 0, 0, 8}
	entry := func(tag, typ uint16, count, value uint32) []byte {
		e := make([]byte, 12)
		be.PutUint16(e[0:], tag)
		be.PutUint16(e[2:], typ)
		be.PutUint32(e[4:], count)
		be.PutUint32(e[8:], value)
		return e
	}
	// IFD0 at 8: 2 entries → 2+24+4 = 30 bytes → GPS IFD at 38.
	t = append(t, 0, 2)
	t = append(t, entry(0x0112, 3, 1, uint32(orientation)<<16)...)
	t = append(t, entry(0x8825, 4, 1, 38)...)
	t = append(t, 0, 0, 0, 0)
	// GPS IFD at 38: 2 entries → rationals at 68.
	t = append(t, 0, 2)
	t = append(t, entry(0x0001, 2, 2, uint32('S')<<24)...)
	t = append(t, entry(0x0002, 5, 3, 68)...)
	t = append(t, 0, 0, 0, 0)
	t = append(t, gpsRationals...)
	return append([]byte("Exif\x00\x00"), t...)
}

// pngChunkBytes builds a PNG chunk with a valid CRC.
func pngChunkBytes(typ string, data []byte) []byte {
	c := make([]byte, 8, 12+len(data))
	binary.BigEndian.PutUint32(c, uint32(len(data)))
	copy(c[4:], typ)
	c = append(c, data...)
	crc := crc32.ChecksumIEEE(c[4:])
	return binary.BigEndian.AppendUint32(c, crc)
}

// insertPNGChunk inserts a chunk right after IHDR (offset 8+25).
func insertPNGChunk(p []byte, typ string, data []byte) []byte {
	out := append([]byte{}, p[:33]...)
	out = append(out, pngChunkBytes(typ, data)...)
	return append(out, p[33:]...)
}
