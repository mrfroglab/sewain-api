// Package crypto provides envelope encryption (AES-256-GCM with a rotatable key ring) and keyed hashing
// (HMAC-SHA256) for identity data: KTP/selfie files, NIK, and OTP codes.
//
// Ciphertext layout (all binary):
//
//	"SWE" | version(1)=0x01 | len(keyID)(1) | keyID | nonce(12) | AES-GCM(ciphertext || tag)
//
// The key ID travels with the ciphertext, so adding a new key (rotation) never breaks data sealed with an
// older key: Open looks the key up by ID, Seal always uses the active (last) key.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	keySize   = 32
	nonceSize = 12
	version1  = 0x01
)

var magic = []byte("SWE")

// ErrUnknownKey is returned by Open when the ciphertext names a key ID that is not in the ring.
var ErrUnknownKey = errors.New("crypto: unknown key id")

// ErrMalformed is returned by Open for data that is not a ciphertext produced by Seal.
var ErrMalformed = errors.New("crypto: malformed ciphertext")

// KeyRing holds AES-256 keys by ID. The last key added is the active one used for new ciphertexts.
type KeyRing struct {
	aeads  map[string]cipher.AEAD
	active string
	ids    []string
}

// ParseKeyRing parses "v1:<base64 32 bytes>,v2:<base64 32 bytes>". The last entry is the active key.
// Key IDs are 1–32 characters of [A-Za-z0-9_-].
func ParseKeyRing(spec string) (*KeyRing, error) {
	kr := &KeyRing{aeads: map[string]cipher.AEAD{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, b64, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("crypto: key entry %q must be <id>:<base64>", redact(part))
		}
		key, err := DecodeKey(b64)
		if err != nil {
			return nil, fmt.Errorf("crypto: key %q: %w", id, err)
		}
		if err := kr.Add(id, key); err != nil {
			return nil, err
		}
	}
	if kr.active == "" {
		return nil, errors.New("crypto: key ring is empty")
	}
	return kr, nil
}

// Add appends a key and makes it the active key.
func (kr *KeyRing) Add(id string, key []byte) error {
	if !validKeyID(id) {
		return fmt.Errorf("crypto: invalid key id %q", id)
	}
	if _, dup := kr.aeads[id]; dup {
		return fmt.Errorf("crypto: duplicate key id %q", id)
	}
	if len(key) != keySize {
		return fmt.Errorf("crypto: key %q must be %d bytes, got %d", id, keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	if kr.aeads == nil {
		kr.aeads = map[string]cipher.AEAD{}
	}
	kr.aeads[id] = aead
	kr.ids = append(kr.ids, id)
	kr.active = id
	return nil
}

// ActiveKeyID returns the ID used by Seal.
func (kr *KeyRing) ActiveKeyID() string { return kr.active }

// KeyIDs returns all key IDs in the order they were added.
func (kr *KeyRing) KeyIDs() []string { return append([]string(nil), kr.ids...) }

// Seal encrypts plaintext with the active key. aad (may be nil) is authenticated but not stored; the same
// aad must be passed to Open. Binding ciphertexts to their owner (e.g. submission ID + field) prevents
// swapping ciphertexts between rows.
func (kr *KeyRing) Seal(plaintext, aad []byte) ([]byte, error) {
	aead := kr.aeads[kr.active]
	if aead == nil {
		return nil, errors.New("crypto: no active key")
	}
	id := kr.active
	header := make([]byte, 0, len(magic)+2+len(id)+nonceSize)
	header = append(header, magic...)
	header = append(header, version1, byte(len(id)))
	header = append(header, id...)
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header = append(header, nonce...)
	out := make([]byte, len(header), len(header)+len(plaintext)+aead.Overhead())
	copy(out, header)
	return aead.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts data produced by Seal with any key in the ring.
func (kr *KeyRing) Open(data, aad []byte) ([]byte, error) {
	id, nonce, ct, err := parse(data)
	if err != nil {
		return nil, err
	}
	aead := kr.aeads[id]
	if aead == nil {
		return nil, fmt.Errorf("%w %q", ErrUnknownKey, id)
	}
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("crypto: decrypt: %w", err)
	}
	return pt, nil
}

// KeyID returns the key ID a ciphertext was sealed with (useful to find data that still needs re-encryption
// after a rotation).
func KeyID(data []byte) (string, error) {
	id, _, _, err := parse(data)
	return id, err
}

// Rewrap re-encrypts data under the active key if it was sealed with an older one. It returns the input
// unchanged (and false) when it already uses the active key.
func (kr *KeyRing) Rewrap(data, aad []byte) ([]byte, bool, error) {
	id, err := KeyID(data)
	if err != nil {
		return nil, false, err
	}
	if id == kr.active {
		return data, false, nil
	}
	pt, err := kr.Open(data, aad)
	if err != nil {
		return nil, false, err
	}
	out, err := kr.Seal(pt, aad)
	return out, err == nil, err
}

func parse(data []byte) (id string, nonce, ct []byte, err error) {
	if len(data) < len(magic)+2 || string(data[:len(magic)]) != string(magic) || data[len(magic)] != version1 {
		return "", nil, nil, ErrMalformed
	}
	n := int(data[len(magic)+1])
	rest := data[len(magic)+2:]
	if n == 0 || len(rest) < n+nonceSize+16 {
		return "", nil, nil, ErrMalformed
	}
	return string(rest[:n]), rest[n : n+nonceSize], rest[n+nonceSize:], nil
}

// HMAC returns HMAC-SHA256(key, parts joined by a 0x00 separator).
func HMAC(key []byte, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{0})
		}
		m.Write(p)
	}
	return m.Sum(nil)
}

// Equal compares two MACs in constant time.
func Equal(a, b []byte) bool { return hmac.Equal(a, b) }

// SHA256 returns the SHA-256 digest of b.
func SHA256(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// RandomBytes returns n bytes from crypto/rand.
func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// DecodeKey decodes a base64 (standard or URL alphabet, padded or not) 32-byte key.
func DecodeKey(s string) ([]byte, error) {
	b, err := decodeB64(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("not valid base64")
	}
	if len(b) != keySize {
		return nil, fmt.Errorf("must decode to %d bytes, got %d (generate with: openssl rand -base64 32)", keySize, len(b))
	}
	return b, nil
}

// DecodeSecret decodes a base64 secret (pepper) of at least 32 bytes.
func DecodeSecret(s string) ([]byte, error) {
	b, err := decodeB64(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("not valid base64")
	}
	if len(b) < keySize {
		return nil, fmt.Errorf("must decode to at least %d bytes, got %d (generate with: openssl rand -base64 32)", keySize, len(b))
	}
	return b, nil
}

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

func validKeyID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func redact(entry string) string {
	if id, _, ok := strings.Cut(entry, ":"); ok {
		return id + ":…"
	}
	if len(entry) > 4 {
		return entry[:4] + "…"
	}
	return entry
}
