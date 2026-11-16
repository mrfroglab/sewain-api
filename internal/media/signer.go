package media

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultURLTTL is how long a signed photo URL stays valid.
	DefaultURLTTL = time.Hour
	// urlBucket quantises expiry times so that the URL for one (photo, viewer) pair stays identical for a
	// while. Snapshots are refetched often; a stable src keeps the browser cache and the snapshot ETag warm.
	// A URL therefore lives between ttl and ttl+urlBucket.
	urlBucket = 15 * time.Minute
	// MinSigningKeyLen is the minimum MEDIA_SIGNING_KEY length in bytes.
	MinSigningKeyLen = 32
	// VariantWatermark is the v= value for the watermarked display copy.
	VariantWatermark = "wm"

	photoPathPrefix = "/api/v1/photos/"
)

var (
	ErrSignatureMissing = errors.New("media: signature missing")
	ErrSignatureInvalid = errors.New("media: signature invalid")
	ErrSignatureExpired = errors.New("media: signature expired")
)

// Signer issues and checks HMAC-SHA256 signed photo URLs. <img> cannot send an Authorization header, so
// the capability travels in the query string (ROADMAP D8).
type Signer struct {
	keys [][]byte // keys[0] signs; all verify (rotation)
	now  func() time.Time
}

// NewSigner takes the current key and optionally previous keys still accepted during a rotation.
func NewSigner(key []byte, previous ...[]byte) (*Signer, error) {
	keys := append([][]byte{key}, previous...)
	for _, k := range keys {
		if len(k) < MinSigningKeyLen {
			return nil, errors.New("media: signing key must be at least 32 bytes")
		}
	}
	return &Signer{keys: keys, now: time.Now}, nil
}

// SignerFromEnv reads MEDIA_SIGNING_KEY (and the optional MEDIA_SIGNING_KEY_PREVIOUS). Values may be hex,
// base64, or a raw string; after decoding they must be at least 32 bytes.
func SignerFromEnv() (*Signer, error) {
	raw := os.Getenv("MEDIA_SIGNING_KEY")
	if raw == "" {
		return nil, errors.New("media: MEDIA_SIGNING_KEY is not set")
	}
	var prev [][]byte
	if p := os.Getenv("MEDIA_SIGNING_KEY_PREVIOUS"); p != "" {
		prev = append(prev, decodeKey(p))
	}
	return NewSigner(decodeKey(raw), prev...)
}

func decodeKey(s string) []byte {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) >= MinSigningKeyLen {
		return b
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) >= MinSigningKeyLen {
			return b
		}
	}
	return []byte(s)
}

// SetClock replaces the clock (tests, and the core's demo clock if wanted).
func (s *Signer) SetClock(now func() time.Time) { s.now = now }

// URLOption customizes SignURL.
type URLOption func(*urlOpts)

type urlOpts struct{ variant string }

// Watermarked asks for the watermarked display copy (&v=wm). The variant is covered by the signature, so
// a viewer cannot strip it to get another rendition.
func Watermarked() URLOption { return func(o *urlOpts) { o.variant = VariantWatermark } }

// SignURL returns "/api/v1/photos/{id}?exp=…&u=…&sig=…" (plus "&v=wm" when Watermarked). ttl ≤ 0 means
// DefaultURLTTL. The caller must have checked CanView for this viewer first (or use SignFor).
// viewerID may be empty for guests; it is bound into the signature for audit, not re-authorized.
func (s *Signer) SignURL(photoID, viewerID string, ttl time.Duration, opts ...URLOption) string {
	var o urlOpts
	for _, f := range opts {
		f(&o)
	}
	if ttl <= 0 {
		ttl = DefaultURLTTL
	}
	exp := s.now().Add(ttl).Unix()
	b := int64(urlBucket / time.Second)
	exp = (exp + b - 1) / b * b
	expS := strconv.FormatInt(exp, 10)
	q := "exp=" + expS
	if viewerID != "" {
		q += "&u=" + url.QueryEscape(viewerID)
	}
	if o.variant != "" {
		q += "&v=" + o.variant
	}
	q += "&sig=" + s.mac(s.keys[0], photoID, expS, viewerID, o.variant)
	return photoPathPrefix + url.PathEscape(photoID) + "?" + q
}

// Claims are what a valid signature vouches for.
type Claims struct {
	PhotoID  string
	ViewerID string
	Variant  string
	Expires  time.Time
}

// Verify checks the query of a photo request against photoID. It fails closed: a missing parameter, any
// changed byte, an unknown variant or a past expiry is rejected. MAC comparison is constant-time.
func (s *Signer) Verify(photoID string, q url.Values) (Claims, error) {
	expS, sig := q.Get("exp"), q.Get("sig")
	if expS == "" || sig == "" {
		return Claims{}, ErrSignatureMissing
	}
	exp, err := strconv.ParseInt(expS, 10, 64)
	if err != nil || strconv.FormatInt(exp, 10) != expS {
		return Claims{}, ErrSignatureInvalid
	}
	viewer, variant := q.Get("u"), q.Get("v")
	if variant != "" && variant != VariantWatermark {
		return Claims{}, ErrSignatureInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return Claims{}, ErrSignatureInvalid
	}
	ok := false
	for _, k := range s.keys {
		want, _ := base64.RawURLEncoding.DecodeString(s.mac(k, photoID, expS, viewer, variant))
		if hmac.Equal(got, want) {
			ok = true
		}
	}
	if !ok {
		return Claims{}, ErrSignatureInvalid
	}
	expires := time.Unix(exp, 0)
	if !s.now().Before(expires) {
		return Claims{}, ErrSignatureExpired
	}
	return Claims{PhotoID: photoID, ViewerID: viewer, Variant: variant, Expires: expires}, nil
}

// mac covers every field with an unambiguous framing (length-prefixed), so "ab"+"c" ≠ "a"+"bc".
func (s *Signer) mac(key []byte, photoID, exp, viewer, variant string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("sewain-photo-v1"))
	for _, f := range []string{photoID, exp, viewer, variant} {
		h.Write([]byte(strconv.Itoa(len(f)) + ":" + f + ";"))
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// SignFor applies the access policy and signs in one step. It returns "" when the viewer may not see the
// photo, so a snapshot can never carry a URL for evidence the viewer is not party to. Public (referensi)
// photos get the plain unsigned path, which is cacheable by everyone. Evidence photos get the watermarked
// variant unless the viewer is an admin (mediators compare originals, FR-P9).
func (s *Signer) SignFor(viewer *Viewer, photo Photo, booking *Booking, ttl time.Duration) string {
	if !CanView(viewer, photo, booking) {
		return ""
	}
	if IsPublicStage(photo.Stage) {
		return photoPathPrefix + url.PathEscape(photo.ID)
	}
	var opts []URLOption
	if !viewer.Admin {
		opts = append(opts, Watermarked())
	}
	return s.SignURL(photo.ID, viewer.ID, ttl, opts...)
}
