package middleware

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// Redaction rules. Phone numbers and credentials must never reach the logs (UU PDP, ROADMAP F5.1):
//   - attributes whose key names a secret (token, authorization, cookie, otp, password, ...) are replaced
//     entirely;
//   - attributes whose key names a phone number keep only a short prefix and the last 3 digits;
//   - any other string value is scanned for Indonesian phone numbers, "Bearer <token>" and
//     "token=<value>"-style query parameters, which are masked in place.
var (
	phoneRe       = regexp.MustCompile(`(?:\+62|\b62|\b0)[ -]?8[1-9](?:[ -]?\d){6,11}\b`)
	bearerRe      = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	secretParamRe = regexp.MustCompile(`(?i)\b(token|access_token|refresh_token|session|otp|code|password|secret|sig|signature)=([^&\s"]+)`)
)

const redacted = "[REDACTED]"

var secretKeys = []string{"token", "authorization", "cookie", "password", "passwd", "secret", "otp", "session", "apikey", "api_key", "signature", "nik"}
var phoneKeys = []string{"phone", "msisdn", "whatsapp", "hp"}

func keyMatches(key string, list []string) bool {
	k := strings.ToLower(key)
	for _, s := range list {
		if k == s || strings.HasSuffix(k, "_"+s) || strings.HasSuffix(k, "."+s) || strings.HasPrefix(k, s+"_") ||
			(len(s) > 3 && strings.Contains(k, s)) {
			return true
		}
	}
	return false
}

// MaskPhone keeps a short prefix and the last 3 digits: "+6281234567890" → "+6281*******890".
func MaskPhone(s string) string {
	var digits []byte
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			digits = append(digits, s[i])
		}
	}
	if len(digits) < 7 {
		return strings.Repeat("*", len(digits))
	}
	prefix := ""
	if strings.HasPrefix(strings.TrimSpace(s), "+") {
		prefix = "+"
	}
	keep := 4
	if digits[0] == '0' {
		keep = 3
	}
	return prefix + string(digits[:keep]) + strings.Repeat("*", len(digits)-keep-3) + string(digits[len(digits)-3:])
}

// RedactString masks phone numbers and credentials embedded in free text.
func RedactString(s string) string {
	s = bearerRe.ReplaceAllString(s, "$1 "+redacted)
	s = secretParamRe.ReplaceAllString(s, "$1="+redacted)
	return phoneRe.ReplaceAllStringFunc(s, MaskPhone)
}

// RedactAttr applies the redaction rules to one attribute (recursing into groups). It is usable as
// slog.HandlerOptions.ReplaceAttr.
func RedactAttr(_ []string, a slog.Attr) slog.Attr {
	return redactAttr(a)
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		attrs := v.Group()
		out := make([]slog.Attr, len(attrs))
		for i, ga := range attrs {
			out[i] = redactAttr(ga)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	}
	// request_id and similar are ours and safe; skip the generic scan for well-known non-PII keys.
	switch {
	case a.Key == "request_id" || a.Key == "user_id" || a.Key == "route" || a.Key == "method":
		return slog.Attr{Key: a.Key, Value: v}
	case keyMatches(a.Key, secretKeys):
		if v.Kind() == slog.KindString && v.String() == "" {
			return slog.Attr{Key: a.Key, Value: v}
		}
		return slog.String(a.Key, redacted)
	case keyMatches(a.Key, phoneKeys):
		if v.Kind() == slog.KindString {
			return slog.String(a.Key, MaskPhone(v.String()))
		}
		return slog.String(a.Key, redacted)
	}
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactString(v.String()))
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, RedactString(err.Error()))
		}
		if st, ok := v.Any().(interface{ String() string }); ok {
			return slog.String(a.Key, RedactString(st.String()))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// RedactingHandler wraps any slog.Handler and masks phone numbers and credentials in the message and in
// every attribute, including ones added with Logger.With. Wrap the process logger with it once in main so
// that logs from every package are covered, not only access logs.
type RedactingHandler struct {
	inner slog.Handler
}

// NewRedactingHandler wraps h.
func NewRedactingHandler(h slog.Handler) *RedactingHandler { return &RedactingHandler{inner: h} }

// Enabled reports whether the inner handler handles level.
func (h *RedactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle redacts the record and passes it on.
func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, RedactString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

// WithAttrs redacts attrs before handing them to the inner handler.
func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return &RedactingHandler{inner: h.inner.WithAttrs(out)}
}

// WithGroup opens a group on the inner handler.
func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: h.inner.WithGroup(name)}
}

// ParseLevel maps LOG_LEVEL (debug|info|warn|error, case-insensitive) to a slog level, defaulting to info.
func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return slog.LevelInfo
	}
	return l
}
