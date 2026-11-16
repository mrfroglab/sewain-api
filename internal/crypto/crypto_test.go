package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func key(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }

func TestSealOpenRoundTrip(t *testing.T) {
	kr, err := ParseKeyRing("v1:" + key(1))
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte("3404010101900001")
	ct, err := kr.Seal(pt, []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, pt) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := kr.Open(ct, []byte("aad"))
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := kr.Open(ct, []byte("other")); err == nil {
		t.Fatal("open with wrong aad must fail")
	}
	ct[len(ct)-1] ^= 1
	if _, err := kr.Open(ct, []byte("aad")); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestRotationKeepsOldDataReadable(t *testing.T) {
	old, _ := ParseKeyRing("v1:" + key(1))
	ct1, _ := old.Seal([]byte("lama"), nil)

	kr, err := ParseKeyRing("v1:" + key(1) + ", v2:" + key(2))
	if err != nil {
		t.Fatal(err)
	}
	if kr.ActiveKeyID() != "v2" {
		t.Fatalf("active = %s", kr.ActiveKeyID())
	}
	if got, err := kr.Open(ct1, nil); err != nil || string(got) != "lama" {
		t.Fatalf("v1 data unreadable after rotation: %q %v", got, err)
	}
	ct2, _ := kr.Seal([]byte("baru"), nil)
	if id, _ := KeyID(ct2); id != "v2" {
		t.Fatalf("new data sealed with %s", id)
	}
	re, changed, err := kr.Rewrap(ct1, nil)
	if err != nil || !changed {
		t.Fatalf("rewrap: %v %v", changed, err)
	}
	if id, _ := KeyID(re); id != "v2" {
		t.Fatalf("rewrapped with %s", id)
	}
	// A ring without v1 cannot read v1 data, and says why.
	only2, _ := ParseKeyRing("v2:" + key(2))
	if _, err := only2.Open(ct1, nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
}

func TestParseKeyRingErrors(t *testing.T) {
	for _, spec := range []string{
		"",
		"v1",
		"v1:notbase64!!",
		"v1:" + base64.StdEncoding.EncodeToString([]byte("short")),
		"v1:" + key(1) + ",v1:" + key(2),
		"bad id:" + key(1),
	} {
		if _, err := ParseKeyRing(spec); err == nil {
			t.Errorf("ParseKeyRing(%q) should fail", spec)
		} else if strings.Contains(err.Error(), key(1)) {
			t.Errorf("error leaks key material: %v", err)
		}
	}
}

func TestMalformed(t *testing.T) {
	kr, _ := ParseKeyRing("v1:" + key(1))
	for _, d := range [][]byte{nil, []byte("SWE"), []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")} {
		if _, err := kr.Open(d, nil); !errors.Is(err, ErrMalformed) {
			t.Errorf("Open(%q) = %v", d, err)
		}
	}
}

func TestHMAC(t *testing.T) {
	a := HMAC([]byte("k"), []byte("ab"), []byte("c"))
	b := HMAC([]byte("k"), []byte("a"), []byte("bc"))
	if Equal(a, b) {
		t.Fatal("part boundaries must matter")
	}
	if !Equal(a, HMAC([]byte("k"), []byte("ab"), []byte("c"))) {
		t.Fatal("hmac not deterministic")
	}
	if Equal(a, HMAC([]byte("k2"), []byte("ab"), []byte("c"))) {
		t.Fatal("key must matter")
	}
}

func TestDecodeSecret(t *testing.T) {
	if _, err := DecodeSecret(key(9)); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSecret("c2hvcnQ="); err == nil {
		t.Fatal("short secret must fail")
	}
}
