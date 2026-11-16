package media

import (
	"bytes"
	"encoding/binary"
	"math"
)

var (
	exifHeader   = []byte("Exif\x00\x00")
	xmpHeader    = []byte("http://ns.adobe.com/xap/1.0/\x00")
	xmpExtHeader = []byte("http://ns.adobe.com/xmp/extension/\x00")
)

// StripGPS removes location-bearing metadata from a public listing photo: EXIF and XMP (APP1) in JPEG,
// eXIf and text chunks in PNG, EXIF/XMP chunks in WebP. Pixel data is copied untouched, nothing is
// re-encoded. For JPEG the EXIF orientation is kept by writing a minimal EXIF block with only that tag,
// so a phone photo does not suddenly display sideways.
//
// Evidence photos must NOT go through this: they are stored byte-for-byte so the hash matches what the
// client sent (FR-P3). Call Inspect on the result if you need its new hash and size.
func StripGPS(data []byte) ([]byte, error) {
	switch DetectType(data) {
	case TypeJPEG:
		return stripJPEG(data)
	case TypePNG:
		return stripPNG(data)
	case TypeWebP:
		return stripWebP(data)
	}
	return nil, errUnsupported
}

func stripJPEG(data []byte) ([]byte, error) {
	segs, _, err := parseJPEG(data)
	if err != nil {
		return nil, errCorrupt
	}
	out := make([]byte, 0, len(data))
	orientation := 0
	insertAt := -1
	for _, sg := range segs {
		body := data[sg.start:sg.end]
		if sg.marker == 0xE1 && len(body) >= 4 {
			payload := body[4:]
			switch {
			case bytes.HasPrefix(payload, exifHeader):
				if o := exifOrientation(payload[len(exifHeader):]); o > 1 && o <= 8 && orientation == 0 {
					orientation = o
				}
				if insertAt < 0 {
					insertAt = len(out)
				}
				continue
			case bytes.HasPrefix(payload, xmpHeader), bytes.HasPrefix(payload, xmpExtHeader):
				continue
			}
		}
		out = append(out, body...)
	}
	if orientation > 0 { // exifOrientation results are only kept for 2–8
		seg := orientationSegment(orientation)
		out = append(out[:insertAt], append(seg, out[insertAt:]...)...)
	}
	return out, nil
}

// exifOrientation reads tag 0x0112 from IFD0 of a TIFF block; 0 if absent or malformed.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(tiff[2:]) != 42 {
		return 0
	}
	off := int(bo.Uint32(tiff[4:]))
	if off < 8 || off+2 > len(tiff) {
		return 0
	}
	n := int(bo.Uint16(tiff[off:]))
	for i := range n {
		e := off + 2 + i*12
		if e+12 > len(tiff) {
			return 0
		}
		if bo.Uint16(tiff[e:]) == 0x0112 && bo.Uint16(tiff[e+2:]) == 3 {
			return int(bo.Uint16(tiff[e+8:]))
		}
	}
	return 0
}

// orientationSegment builds an APP1 EXIF segment holding only IFD0 with the Orientation tag.
func orientationSegment(o int) []byte {
	if o < 1 || o > 8 {
		o = 1
	}
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, // header, IFD0 at offset 8
		0, 1, // one entry
		0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, byte(o), 0, 0, // #nosec G115 -- o is 1–8; Orientation SHORT ×1
		0, 0, 0, 0} // no next IFD
	payload := append(append([]byte{}, exifHeader...), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2)) // #nosec G115 -- fixed 34-byte payload
	return append(seg, payload...)
}

func stripPNG(data []byte) ([]byte, error) {
	chunks, _, err := parsePNG(data)
	if err != nil {
		return nil, errCorrupt
	}
	out := append(make([]byte, 0, len(data)), data[:8]...)
	for _, c := range chunks {
		switch c.typ {
		case "eXIf", "tEXt", "zTXt", "iTXt":
			continue
		}
		out = append(out, data[c.start:c.end]...)
	}
	return out, nil
}

func stripWebP(data []byte) ([]byte, error) {
	chunks, _, err := parseWebP(data)
	if err != nil {
		return nil, errCorrupt
	}
	out := append(make([]byte, 0, len(data)), data[:12]...)
	for _, c := range chunks {
		switch c.fourCC {
		case "EXIF", "XMP ":
			continue
		case "VP8X":
			chunk := append([]byte{}, data[c.start:c.end]...)
			if len(chunk) > 8 {
				chunk[8] &^= 0x08 | 0x04 // clear EXIF and XMP flags
			}
			out = append(out, chunk...)
			continue
		}
		out = append(out, data[c.start:c.end]...)
	}
	if len(out)-8 > math.MaxUint32 { // cannot happen below MaxUploadBytes, but RIFF sizes are 32 bit
		return nil, errCorrupt
	}
	binary.LittleEndian.PutUint32(out[4:], uint32(len(out)-8)) // #nosec G115 -- range checked above
	return out, nil
}
