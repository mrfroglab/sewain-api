package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Enqueuer turns in-app notifications into outbox rows.
type Enqueuer struct {
	// Channels lists the channels configured on this server (Config.Channels). Others are never queued.
	Channels map[Channel]bool
	// PublicURL is the web origin used to build absolute links, e.g. "https://sewain.hallojanu.me".
	PublicURL string
	// Now defaults to time.Now.
	Now func() time.Time
}

func (e *Enqueuer) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// AbsURL joins PUBLIC_URL and an app path. Absolute hrefs are returned unchanged.
func AbsURL(publicURL, href string) string {
	if href == "" {
		return strings.TrimRight(publicURL, "/") + "/"
	}
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	if !strings.HasPrefix(href, "/") {
		href = "/" + href
	}
	return strings.TrimRight(publicURL, "/") + href
}

// Enqueue queues n on every channel the user should receive it on. Call it inside the core transaction that
// inserts the in-app notification, so both commit or roll back together. It is idempotent per
// (notification id, channel) and returns the channels queued.
//
// Channel rules: the channel is configured on the server, the user's preference for the category is on, and the
// user is reachable there (at least one push subscription; an email address in users.email). During quiet hours
// non-important notifications are scheduled for the end of the window instead of now.
func (e *Enqueuer) Enqueue(ctx context.Context, tx pgx.Tx, n Notification) ([]Channel, error) {
	if n.ID == "" || n.UserID == "" {
		return nil, fmt.Errorf("notify: enqueue needs notification id and user id")
	}
	prefs, err := LoadPrefs(ctx, tx, n.UserID)
	if err != nil {
		return nil, err
	}
	cat := CategoryOf(n.Kind)
	important := n.important()
	now := e.now()
	due := now
	if !important {
		if until := prefs.QuietHours.QuietUntil(now); !until.IsZero() {
			due = until
		}
	}
	tag := n.Tag
	if tag == "" {
		tag = n.ID
	}
	base := Payload{
		Title: n.Title, Body: n.Body, Href: n.Href, URL: AbsURL(e.PublicURL, n.Href), Tag: tag,
		Kind: n.Kind, Category: cat, Important: important,
	}

	var queued []Channel
	for _, ch := range QueuedChannels {
		if !e.Channels[ch] || !prefs.Enabled(ch, cat) {
			continue
		}
		p := base
		switch ch {
		case ChannelWebPush:
			var has bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM push_subscriptions WHERE user_id = $1)`, n.UserID).Scan(&has); err != nil {
				return nil, fmt.Errorf("notify: push subscriptions: %w", err)
			}
			if !has {
				continue
			}
		case ChannelEmail:
			var email string
			if err := tx.QueryRow(ctx, `SELECT coalesce(email, '') FROM users WHERE id = $1`, n.UserID).Scan(&email); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return nil, fmt.Errorf("notify: user email: %w", err)
			}
			if email = strings.TrimSpace(email); email == "" {
				continue
			}
			p.To = email
		case ChannelWhatsApp:
			// Stub: reachable only once a paid WhatsApp provider is wired (ROADMAP §9 #9).
		}
		body, _ := json.Marshal(p)
		tag, err := tx.Exec(ctx, `
			INSERT INTO notification_outbox (user_id, notification_id, channel, payload, next_attempt_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (notification_id, channel) DO NOTHING`, n.UserID, n.ID, string(ch), body, due, now)
		if err != nil {
			return nil, fmt.Errorf("notify: enqueue %s: %w", ch, err)
		}
		if tag.RowsAffected() == 1 {
			queued = append(queued, ch)
		}
	}
	return queued, nil
}
