package auth

import (
	"net/http"
	"strings"
)

// NormalizePhone converts an Indonesian mobile number ("0812-3456-7890", "62812…", "+62 812…", "812…")
// to E.164 ("+628123456789"). Only mobile numbers (prefix 8 after the country code) are accepted.
func NormalizePhone(raw string) (string, error) {
	var b strings.Builder
	for i, r := range strings.TrimSpace(raw) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", errBadPhone
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(d, "62"):
		d = d[2:]
	case strings.HasPrefix(d, "0"):
		d = d[1:]
	}
	// National significant number for Indonesian mobiles: 8xx…, 9–12 digits.
	if !strings.HasPrefix(d, "8") || len(d) < 9 || len(d) > 12 {
		return "", errBadPhone
	}
	return "+62" + d, nil
}

var errBadPhone = userErr(http.StatusBadRequest, "Nomor HP tidak valid. Pakai nomor Indonesia, contoh 0812xxxxxxx.")
