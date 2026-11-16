package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"
)

// Upload limits (F3.2).
const (
	MaxUploadBytes = 10 << 20
	MaxDimension   = 8000
	MinDimension   = 32
	// MaxDecodeBytes bounds the memory one full decode may take, estimated from the header: width × height ×
	// bytes per pixel (at least 4, the RGBA canvas the watermark draws on). The API runs with a 256 MB
	// memory limit, so 64 MB (e.g. 4096 × 4096 at 8 bit) leaves room for the decoded image plus the
	// watermark canvas. A tiny compressed file can declare a huge image (decompression bomb), which is why
	// the dimension check alone is not enough.
	MaxDecodeBytes = 64 << 20
	// MaxConcurrentDecodes is how many full decodes (upload validation, watermark rendering) may run at once
	// in this process. Together with MaxDecodeBytes it caps the pixel memory at roughly 2 × MaxDecodeBytes.
	MaxConcurrentDecodes = 1
)

// decodeSlots serializes full decodes; decodeHook lets tests observe them.
var (
	decodeSlots = make(chan struct{}, MaxConcurrentDecodes)
	decodeHook  func() (done func())
)

func acquireDecode() (release func()) {
	decodeSlots <- struct{}{}
	var done func()
	if decodeHook != nil {
		done = decodeHook()
	}
	return func() {
		if done != nil {
			done()
		}
		<-decodeSlots
	}
}

// decodeCost estimates the bytes a full decode of cfg allocates, counting at least 4 bytes per pixel for
// the RGBA canvas used by Watermark.
func decodeCost(cfg image.Config) int64 {
	bpp := int64(4)
	switch cfg.ColorModel {
	case color.RGBA64Model, color.NRGBA64Model:
		bpp = 8
	}
	return int64(cfg.Width) * int64(cfg.Height) * bpp
}

// decodeConfig reads only the image header.
func decodeConfig(typ string, data []byte) (image.Config, error) {
	switch typ {
	case TypeJPEG:
		return jpeg.DecodeConfig(bytes.NewReader(data))
	case TypePNG:
		return png.DecodeConfig(bytes.NewReader(data))
	case TypeWebP:
		return webp.DecodeConfig(bytes.NewReader(data))
	}
	return image.Config{}, errUnsupported
}

// Image types accepted for upload.
const (
	TypeJPEG = "jpeg"
	TypePNG  = "png"
	TypeWebP = "webp"
)

// Info describes a validated upload.
type Info struct {
	Type   string // jpeg | png | webp
	MIME   string // image/jpeg | image/png | image/webp
	Ext    string // .jpg | .png | .webp
	Width  int
	Height int
	Bytes  int64
	SHA256 string // hex SHA-256 of exactly the bytes received (FR-P3)
}

// UploadError is a validation failure with a message that can be shown to the user as is.
type UploadError struct {
	Status int // suggested HTTP status
	Msg    string
}

func (e *UploadError) Error() string { return e.Msg }

func uploadErr(status int, msg string) error { return &UploadError{Status: status, Msg: msg} }

var (
	errTooLarge      = uploadErr(413, "Foto terlalu besar (maks 10 MB).")
	errEmpty         = uploadErr(400, "File foto kosong.")
	errUnsupported   = uploadErr(415, "Format foto tidak didukung. Pakai JPEG, PNG, atau WebP.")
	errCorrupt       = uploadErr(400, "File foto rusak atau bukan gambar yang valid.")
	errTrailing      = uploadErr(400, "File foto berisi data tambahan yang tidak dikenal. Ambil ulang fotonya, ya.")
	errMarkup        = uploadErr(400, "File foto berisi kode/skrip tersembunyi, jadi ditolak.")
	errTooSmall      = uploadErr(400, "Foto terlalu kecil (minimal 32 × 32 piksel).")
	errTooWide       = uploadErr(400, "Dimensi foto terlalu besar (maks 8000 piksel per sisi).")
	errTooManyPixels = uploadErr(400, "Resolusi foto terlalu besar (maks sekitar 16 megapiksel). Kecilkan dulu fotonya, ya.")
)

var mimeOf = map[string]string{TypeJPEG: "image/jpeg", TypePNG: "image/png", TypeWebP: "image/webp"}
var extOf = map[string]string{TypeJPEG: ".jpg", TypePNG: ".png", TypeWebP: ".webp"}

// DetectType returns jpeg, png or webp from the magic bytes, or "" for anything else. The file name and
// the client's Content-Type are never trusted.
func DetectType(data []byte) string {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return TypeJPEG
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return TypePNG
	case len(data) >= 16 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" &&
		(string(data[12:16]) == "VP8 " || string(data[12:16]) == "VP8L" || string(data[12:16]) == "VP8X"):
		return TypeWebP
	}
	return ""
}

// MIMEType maps a DetectType result to its media type ("" if unknown).
func MIMEType(typ string) string { return mimeOf[typ] }

// markupNeedles catch HTML/SVG/script/PHP payloads hidden in image metadata (polyglots that a browser or a
// misconfigured server could interpret). Matching is case-insensitive.
var markupNeedles = [][]byte{
	[]byte("<script"), []byte("<html"), []byte("<svg"), []byte("<?php"), []byte("<iframe"),
	[]byte("<body"), []byte("<!doctype"), []byte("javascript:"), []byte("<object"), []byte("<embed"),
}

func containsMarkup(data []byte) bool {
	// Scan in chunks to avoid lowering a full 10 MB copy; needles are ≤ 11 bytes, overlap covers splits.
	const chunk, overlap = 64 << 10, 16
	buf := make([]byte, 0, chunk+overlap)
	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk+overlap, len(data))
		buf = buf[:0]
		for _, c := range data[off:end] {
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			buf = append(buf, c)
		}
		for _, n := range markupNeedles {
			if bytes.Contains(buf, n) {
				return true
			}
		}
	}
	return false
}

// Inspect validates an upload: size ≤ 10 MB, JPEG/PNG/WebP by magic bytes, a complete decodable image with
// both sides in [32, 8000] px, no bytes after the image's end marker and no embedded markup. It hashes the
// bytes as received. Errors are *UploadError with a message in Indonesian.
func Inspect(data []byte) (Info, error) {
	if len(data) > MaxUploadBytes {
		return Info{}, errTooLarge
	}
	if len(data) == 0 {
		return Info{}, errEmpty
	}
	typ := DetectType(data)
	if typ == "" {
		return Info{}, errUnsupported
	}
	// Header first: reject absurd dimensions before allocating pixels (decompression bombs).
	cfg, err := decodeConfig(typ, data)
	if err != nil {
		return Info{}, errCorrupt
	}
	if cfg.Width > MaxDimension || cfg.Height > MaxDimension {
		return Info{}, errTooWide
	}
	if cfg.Width < MinDimension || cfg.Height < MinDimension {
		return Info{}, errTooSmall
	}
	if decodeCost(cfg) > MaxDecodeBytes {
		return Info{}, errTooManyPixels
	}
	meta, err := checkContainer(typ, data)
	if err != nil {
		return Info{}, err
	}
	for _, r := range meta {
		if containsMarkup(data[r[0]:r[1]]) {
			return Info{}, errMarkup
		}
	}
	// Full decode proves the pixel data is complete and well-formed, not just the header.
	release := acquireDecode()
	_, err = decodeImage(typ, data)
	release()
	if err != nil {
		return Info{}, errCorrupt
	}
	sum := sha256.Sum256(data)
	return Info{
		Type: typ, MIME: mimeOf[typ], Ext: extOf[typ],
		Width: cfg.Width, Height: cfg.Height, Bytes: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func decodeImage(typ string, data []byte) (image.Image, error) {
	switch typ {
	case TypeJPEG:
		return jpeg.Decode(bytes.NewReader(data))
	case TypePNG:
		return png.Decode(bytes.NewReader(data))
	case TypeWebP:
		return webp.Decode(bytes.NewReader(data))
	}
	return nil, errUnsupported
}

// checkContainer walks the file structure and rejects anything appended after the image (ZIP/HTML
// polyglots, "motion photo" videos); zero padding after the end marker is tolerated. It returns the byte
// ranges that hold metadata (JPEG APPn/COM segments, PNG/WebP non-pixel chunks), where a hidden payload
// would live. Compressed pixel data is not scanned: it is random-looking and would give false positives.
func checkContainer(typ string, data []byte) ([][2]int, error) {
	var end int
	var meta [][2]int
	switch typ {
	case TypeJPEG:
		segs, e, err := parseJPEG(data)
		if err != nil {
			return nil, errCorrupt
		}
		end = e
		for _, sg := range segs {
			if sg.marker >= 0xE0 && sg.marker <= 0xEF || sg.marker == 0xFE {
				meta = append(meta, [2]int{sg.start, sg.end})
			}
		}
	case TypePNG:
		chunks, e, err := parsePNG(data)
		if err != nil {
			return nil, errCorrupt
		}
		end = e
		for _, c := range chunks {
			if c.typ != "IDAT" {
				meta = append(meta, [2]int{c.start, c.end})
			}
		}
	case TypeWebP:
		chunks, e, err := parseWebP(data)
		if err != nil {
			return nil, errCorrupt
		}
		end = e
		for _, c := range chunks {
			if c.fourCC != "VP8 " && c.fourCC != "VP8L" && c.fourCC != "ALPH" && c.fourCC != "ANMF" {
				meta = append(meta, [2]int{c.start, c.end})
			}
		}
	}
	for _, b := range data[end:] {
		if b != 0 {
			return nil, errTrailing
		}
	}
	return meta, nil
}

// --- container parsers -------------------------------------------------------------------------------

var errFormat = errors.New("media: malformed image container")

type jpegSegment struct {
	marker     byte
	start, end int // byte range of the whole segment including the FF xx marker
}

// parseJPEG splits a JPEG into marker segments (entropy-coded data is folded into the SOS segment) and
// returns the offset just past EOI.
func parseJPEG(data []byte) ([]jpegSegment, int, error) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, 0, errFormat
	}
	segs := []jpegSegment{{marker: 0xD8, start: 0, end: 2}}
	i := 2
	for {
		// Markers may be preceded by any number of 0xFF fill bytes.
		if i >= len(data) || data[i] != 0xFF {
			return nil, 0, errFormat
		}
		start := i
		for i < len(data) && data[i] == 0xFF {
			i++
		}
		if i >= len(data) {
			return nil, 0, errFormat
		}
		m := data[i]
		i++
		switch {
		case m == 0xD9: // EOI
			segs = append(segs, jpegSegment{marker: m, start: start, end: i})
			return segs, i, nil
		case m == 0x01 || (m >= 0xD0 && m <= 0xD7): // standalone markers
			segs = append(segs, jpegSegment{marker: m, start: start, end: i})
			continue
		}
		if i+2 > len(data) {
			return nil, 0, errFormat
		}
		n := int(binary.BigEndian.Uint16(data[i:]))
		if n < 2 || i+n > len(data) {
			return nil, 0, errFormat
		}
		i += n
		if m == 0xDA { // SOS: skip entropy-coded data up to the next real marker
			for {
				if i+1 >= len(data) {
					return nil, 0, errFormat
				}
				if data[i] == 0xFF && data[i+1] != 0x00 && (data[i+1] < 0xD0 || data[i+1] > 0xD7) && data[i+1] != 0xFF {
					break
				}
				i++
			}
		}
		segs = append(segs, jpegSegment{marker: m, start: start, end: i})
	}
}

type pngChunk struct {
	typ        string
	start, end int
}

func parsePNG(data []byte) ([]pngChunk, int, error) {
	var chunks []pngChunk
	i := 8
	for i+12 <= len(data) {
		n := int(binary.BigEndian.Uint32(data[i:]))
		if n < 0 || n > len(data) || i+12+n > len(data) {
			return nil, 0, errFormat
		}
		t := string(data[i+4 : i+8])
		chunks = append(chunks, pngChunk{typ: t, start: i, end: i + 12 + n})
		i += 12 + n
		if t == "IEND" {
			return chunks, i, nil
		}
	}
	return nil, 0, errFormat
}

type riffChunk struct {
	fourCC     string
	start, end int // includes header and padding byte
}

func parseWebP(data []byte) ([]riffChunk, int, error) {
	if len(data) < 12 {
		return nil, 0, errFormat
	}
	size := int(binary.LittleEndian.Uint32(data[4:]))
	end := 8 + size
	if size < 4 || end > len(data) {
		return nil, 0, errFormat
	}
	var chunks []riffChunk
	i := 12
	for i < end {
		if i+8 > end {
			return nil, 0, errFormat
		}
		n := int(binary.LittleEndian.Uint32(data[i+4:]))
		next := i + 8 + n + n&1
		if n < 0 || next > end || next < i {
			return nil, 0, errFormat
		}
		chunks = append(chunks, riffChunk{fourCC: string(data[i : i+4]), start: i, end: next})
		i = next
	}
	return chunks, end, nil
}
