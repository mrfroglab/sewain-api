package media

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testKey = bytes.Repeat([]byte("k"), 32)

func newTestSigner(t *testing.T, now *time.Time) *Signer {
	t.Helper()
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return *now })
	return s
}

func parseSigned(t *testing.T, raw string) (string, url.Values) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := strings.CutPrefix(u.Path, "/api/v1/photos/")
	if !ok {
		t.Fatalf("unexpected path %q", u.Path)
	}
	return id, u.Query()
}

func TestSignerRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 2, 0, 0, time.UTC)
	s := newTestSigner(t, &now)
	raw := s.SignURL("p-abc", "u-renter", 0)
	if !strings.HasPrefix(raw, "/api/v1/photos/p-abc?exp=") || !strings.Contains(raw, "&sig=") || strings.Contains(raw, "v=wm") {
		t.Fatalf("url = %s", raw)
	}
	id, q := parseSigned(t, raw)
	c, err := s.Verify(id, q)
	if err != nil {
		t.Fatal(err)
	}
	if c.ViewerID != "u-renter" || c.Variant != "" || c.Expires.Sub(now) < DefaultURLTTL || c.Expires.Sub(now) > DefaultURLTTL+urlBucket {
		t.Fatalf("claims = %+v", c)
	}
	// Stable within the bucket: a refetch a minute later yields the same URL.
	later := now.Add(time.Minute)
	s2 := newTestSigner(t, &later)
	if s2.SignURL("p-abc", "u-renter", 0) != raw {
		t.Fatal("URL not stable across a short refetch")
	}

	wm := s.SignURL("p-abc", "u-renter", time.Minute, Watermarked())
	id, q = parseSigned(t, wm)
	if c, err := s.Verify(id, q); err != nil || c.Variant != VariantWatermark {
		t.Fatalf("wm verify = %+v, %v", c, err)
	}
}

func TestSignerRejects(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	s := newTestSigner(t, &now)
	raw := s.SignURL("p-abc", "u-renter", time.Hour, Watermarked())
	id, q := parseSigned(t, raw)

	mutate := func(f func(url.Values)) url.Values {
		c := url.Values{}
		for k, v := range q {
			c[k] = append([]string{}, v...)
		}
		f(c)
		return c
	}
	exp, _ := strconv.ParseInt(q.Get("exp"), 10, 64)
	sig := q.Get("sig")
	flip := func(s string, i int) string {
		b := []byte(s)
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return string(b)
	}

	cases := []struct {
		name string
		id   string
		q    url.Values
		want error
	}{
		{"no sig", id, mutate(func(v url.Values) { v.Del("sig") }), ErrSignatureMissing},
		{"no exp", id, mutate(func(v url.Values) { v.Del("exp") }), ErrSignatureMissing},
		{"wrong sig", id, mutate(func(v url.Values) { v.Set("sig", strings.Repeat("A", len(sig))) }), ErrSignatureInvalid},
		{"sig one char changed", id, mutate(func(v url.Values) { v.Set("sig", flip(sig, len(sig)/2)) }), ErrSignatureInvalid},
		{"sig truncated", id, mutate(func(v url.Values) { v.Set("sig", sig[:len(sig)-1]) }), ErrSignatureInvalid},
		{"sig not base64", id, mutate(func(v url.Values) { v.Set("sig", "!!!") }), ErrSignatureInvalid},
		{"other photo", "p-abd", q, ErrSignatureInvalid},
		{"exp extended by 1s", id, mutate(func(v url.Values) { v.Set("exp", strconv.FormatInt(exp+1, 10)) }), ErrSignatureInvalid},
		{"exp with leading zero", id, mutate(func(v url.Values) { v.Set("exp", "0"+v.Get("exp")) }), ErrSignatureInvalid},
		{"viewer swapped", id, mutate(func(v url.Values) { v.Set("u", "u-attacker") }), ErrSignatureInvalid},
		{"viewer removed", id, mutate(func(v url.Values) { v.Del("u") }), ErrSignatureInvalid},
		{"watermark stripped", id, mutate(func(v url.Values) { v.Del("v") }), ErrSignatureInvalid},
		{"unknown variant", id, mutate(func(v url.Values) { v.Set("v", "orig") }), ErrSignatureInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Verify(c.id, c.q); err != c.want {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}

	t.Run("expired", func(t *testing.T) {
		late := time.Unix(exp, 0)
		s2 := newTestSigner(t, &late)
		if _, err := s2.Verify(id, q); err != ErrSignatureExpired {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("other key", func(t *testing.T) {
		other, _ := NewSigner(bytes.Repeat([]byte("x"), 32))
		other.SetClock(func() time.Time { return now })
		if _, err := other.Verify(id, q); err != ErrSignatureInvalid {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rotation accepts previous key", func(t *testing.T) {
		rotated, _ := NewSigner(bytes.Repeat([]byte("n"), 32), testKey)
		rotated.SetClock(func() time.Time { return now })
		if _, err := rotated.Verify(id, q); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSignerKeyFromEnv(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
	t.Setenv("MEDIA_SIGNING_KEY", "")
	if _, err := SignerFromEnv(); err == nil {
		t.Fatal("empty env accepted")
	}
	t.Setenv("MEDIA_SIGNING_KEY", strings.Repeat("ab", 32)) // hex, 32 bytes
	s, err := SignerFromEnv()
	if err != nil || len(s.keys[0]) != 32 || s.keys[0][0] != 0xab {
		t.Fatalf("hex key: %v", err)
	}
}

func TestSignFor(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	s := newTestSigner(t, &now)
	b := &Booking{ID: "b-1", RenterID: "u-r", ProviderID: "u-p"}
	ev := Photo{ID: "p-1", Stage: StageCheckout, BookingID: "b-1"}
	ref := Photo{ID: "p-ref", Stage: StageReferensi, ListingID: "l-1"}

	if got := s.SignFor(nil, ref, nil, 0); got != "/api/v1/photos/p-ref" {
		t.Fatalf("public = %q", got)
	}
	if got := s.SignFor(&Viewer{ID: "u-x"}, ev, b, 0); got != "" {
		t.Fatalf("stranger got %q", got)
	}
	if got := s.SignFor(nil, ev, b, 0); got != "" {
		t.Fatalf("guest got %q", got)
	}
	if got := s.SignFor(&Viewer{ID: "u-r"}, ev, b, 0); !strings.Contains(got, "v=wm") {
		t.Fatalf("party should get watermarked URL: %q", got)
	}
	if got := s.SignFor(&Viewer{ID: "u-admin", Admin: true}, ev, b, 0); got == "" || strings.Contains(got, "v=wm") {
		t.Fatalf("admin should get the original: %q", got)
	}
}
