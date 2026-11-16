package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// VAPID holds the server's Web Push identity (RFC 8292). Keys are base64url, as produced by GenerateVAPIDKeys.
type VAPID struct {
	PublicKey  string
	PrivateKey string
	Subject    string // "mailto:admin@example.com" or an https URL
}

// Enabled reports whether all fields are set.
func (v VAPID) Enabled() bool { return v.PublicKey != "" && v.PrivateKey != "" && v.Subject != "" }

// GenerateVAPIDKeys creates a new P-256 key pair for VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY.
func GenerateVAPIDKeys() (publicKey, privateKey string, err error) {
	privateKey, publicKey, err = webpush.GenerateVAPIDKeys()
	return publicKey, privateKey, err
}

// MaxPushPayload is the size limit of the JSON shown by the service worker (well under the 4 KB push limit).
const MaxPushPayload = 3 * 1024

// PushMessage is the JSON the service worker receives in the push event.
type PushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Href  string `json:"href"` // app path to open on click
	Tag   string `json:"tag"`
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// EncodePushMessage builds the push JSON from a payload, shortening the body until it fits MaxPushPayload.
func EncodePushMessage(p Payload) []byte {
	href := p.Href
	if href == "" {
		href = "/notifikasi"
	}
	m := PushMessage{Title: truncateRunes(p.Title, 120), Body: truncateRunes(p.Body, 1000), Href: href, Tag: truncateRunes(p.Tag, 64)}
	if len(m.Href) > 512 {
		m.Href = "/notifikasi"
	}
	for {
		b, _ := json.Marshal(m)
		if len(b) <= MaxPushPayload || m.Body == "" {
			return b
		}
		m.Body = truncateRunes(m.Body, max(utf8.RuneCountInString(m.Body)/2, 1))
		if m.Body == "…" {
			m.Body = ""
		}
	}
}

// PushSender sends web push to every subscription of the job's user.
type PushSender struct {
	DB         DB
	VAPID      VAPID
	HTTPClient *http.Client  // default: 15s timeout
	TTL        time.Duration // how long the push service keeps an undelivered message (default 24h)
	Now        func() time.Time
}

// ErrNoSubscriptions means the user has no (more) push subscriptions; the job is dead.
var ErrNoSubscriptions = errors.New("no push subscriptions")

type subscription struct {
	id, endpoint, p256dh, auth string
}

func (s *PushSender) Send(ctx context.Context, job Job) error {
	rows, err := s.DB.Query(ctx, `SELECT id, endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = $1 ORDER BY created_at`, job.UserID)
	if err != nil {
		return err
	}
	var subs []subscription
	for rows.Next() {
		var x subscription
		if err := rows.Scan(&x.id, &x.endpoint, &x.p256dh, &x.auth); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(subs) == 0 {
		return Permanent(ErrNoSubscriptions)
	}

	msg := EncodePushMessage(job.Payload)
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	urgency := webpush.UrgencyNormal
	if job.Payload.Important {
		urgency = webpush.UrgencyHigh
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	var delivered, retryable int
	var lastErr error
	for _, sub := range subs {
		resp, err := webpush.SendNotificationWithContext(ctx, msg, &webpush.Subscription{
			Endpoint: sub.endpoint, Keys: webpush.Keys{P256dh: sub.p256dh, Auth: sub.auth},
		}, &webpush.Options{
			HTTPClient:      client,
			Subscriber:      strings.TrimPrefix(s.VAPID.Subject, "mailto:"), // the library adds mailto: itself
			VAPIDPublicKey:  s.VAPID.PublicKey,
			VAPIDPrivateKey: s.VAPID.PrivateKey,
			TTL:             int(ttl.Seconds()),
			Urgency:         urgency,
		})
		if err != nil {
			retryable++
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			delivered++
			_, _ = s.DB.Exec(ctx, `UPDATE push_subscriptions SET last_success_at = $2 WHERE id = $1`, sub.id, now())
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			// The browser unsubscribed or the subscription expired: forget it.
			_, _ = s.DB.Exec(ctx, `DELETE FROM push_subscriptions WHERE id = $1`, sub.id)
			lastErr = fmt.Errorf("push %s: subscription gone (%d)", hostOf(sub.endpoint), resp.StatusCode)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			retryable++
			lastErr = fmt.Errorf("push %s: %d %s", hostOf(sub.endpoint), resp.StatusCode, strings.TrimSpace(string(body)))
		default: // 400, 401/403 (VAPID mismatch), 413: this subscription will not accept it
			lastErr = fmt.Errorf("push %s: %d %s", hostOf(sub.endpoint), resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	switch {
	case delivered > 0:
		return nil // at least one device got it; retrying would duplicate on that device
	case retryable > 0:
		return lastErr
	default:
		return Permanent(lastErr)
	}
}

func hostOf(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Host
	}
	return "?"
}

// decodeKey accepts base64url (the browser's PushSubscription.toJSON) and standard base64, padded or not.
func decodeKey(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}
