package auth

import "testing"

func TestNormalizePhone(t *testing.T) {
	ok := map[string]string{
		"081234567890":      "+6281234567890",
		"0812-3456-7890":    "+6281234567890",
		"+62 812 3456 7890": "+6281234567890",
		"6281234567890":     "+6281234567890",
		"81234567890":       "+6281234567890",
		"(0812) 345.678":    "+62812345678",
	}
	for in, want := range ok {
		if got, err := NormalizePhone(in); err != nil || got != want {
			t.Errorf("NormalizePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0274123456", "+1 555 1234567", "0812", "08123456789012345", "0812abc4567", "++6281234567"} {
		if got, err := NormalizePhone(in); err == nil {
			t.Errorf("NormalizePhone(%q) = %q, want error", in, got)
		}
	}
}
