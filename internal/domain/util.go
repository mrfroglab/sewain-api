package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // distroless image has no zoneinfo
)

// WIB: dates ("tanggal ambil/kembali") are interpreted in the pilot's local time zone.
var WIB = mustLoc("Asia/Jakarta")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone("WIB", 7*3600)
	}
	return l
}

const (
	Hour = int64(3_600_000)
	Day  = int64(86_400_000)
)

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// Deref returns *p, or def when p is nil.
func Deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

// NewID returns "<prefix>-<random>", matching the web app's id style.
func NewID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + strconv.FormatInt(time.Now().UnixMilli(), 36) + hex.EncodeToString(b)[:7]
}

func IsoDate(ms int64) string { return time.UnixMilli(ms).In(WIB).Format("2006-01-02") }

func ParseIsoDate(iso string) (int64, error) {
	t, err := time.ParseInLocation("2006-01-02", iso, WIB)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}

func EndOfDay(iso string) int64 {
	ms, err := ParseIsoDate(iso)
	if err != nil {
		return 0
	}
	return ms + Day - 1
}

func AddDays(iso string, days int) string {
	t, err := time.ParseInLocation("2006-01-02", iso, WIB)
	if err != nil {
		return iso
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

// DiffDays is the number of rental days between pick-up and return (min 1).
func DiffDays(start, end string) int {
	a, e1 := ParseIsoDate(start)
	b, e2 := ParseIsoDate(end)
	if e1 != nil || e2 != nil {
		return 1
	}
	d := int(math.Round(float64(b-a) / float64(Day)))
	if d < 1 {
		return 1
	}
	return d
}

// Rupiah formats 25000 as "Rp25.000".
func Rupiah(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-Rp" + b.String()
	}
	return "Rp" + b.String()
}

func FirstName(name string) string {
	f := strings.Fields(name)
	if len(f) == 0 {
		return name
	}
	return f[0]
}

// PublicName is what other users see (FR-A5): nickname or first name.
func PublicName(u *User) string {
	if u == nil {
		return "Pengguna"
	}
	if u.Nickname != nil && *u.Nickname != "" {
		return *u.Nickname
	}
	return FirstName(u.Name)
}

func MaskNik(nik string) string {
	if len(nik) < 8 {
		return nik
	}
	return nik[:4] + strings.Repeat("*", len(nik)-8) + nik[len(nik)-4:]
}

// Round1000 rounds to the nearest thousand rupiah.
func Round1000(v float64) int64 { return int64(math.Round(v/1000) * 1000) }

// FmtPercent formats 12.5 as "12,5" and 10 as "10".
func FmtPercent(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.Replace(fmt.Sprintf("%.1f", v), ".", ",", 1)
}

// Truncate cuts s to n runes.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
