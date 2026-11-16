// Package notify delivers in-app notifications to other channels (web push, email, WhatsApp later) through a
// transactional outbox.
//
// The core keeps owning the in-app `notifications` table. In the same transaction that inserts an in-app
// notification it calls Enqueuer.Enqueue, which picks channels from the user's preferences and subscriptions and
// writes rows to notification_outbox. A Dispatcher (one per instance, safe to run on many) claims due rows with
// FOR UPDATE SKIP LOCKED, sends them, and retries with exponential backoff before giving up ("dead").
package notify

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	_ "time/tzdata" // quiet hours need Asia/Jakarta even in minimal container images
)

// Channel is an out-of-app delivery channel. In-app delivery is the core's notifications table and always on.
type Channel string

const (
	ChannelInApp    Channel = "inapp" // preferences only; never queued
	ChannelWebPush  Channel = "webpush"
	ChannelEmail    Channel = "email"
	ChannelWhatsApp Channel = "whatsapp"
)

// QueuedChannels are the channels the outbox can hold, in preference order.
var QueuedChannels = []Channel{ChannelWebPush, ChannelEmail, ChannelWhatsApp}

// Category groups notification kinds for preferences.
type Category string

const (
	CategoryBooking  Category = "booking"
	CategoryChat     Category = "chat"
	CategorySengketa Category = "sengketa"
	CategorySistem   Category = "sistem"
)

// Categories lists every category in display order.
var Categories = []Category{CategoryBooking, CategoryChat, CategorySengketa, CategorySistem}

// CategoryOf maps a core notification kind to its preference category. Kinds used by v1: booking, papan,
// sengketa, sistem, verifikasi. Unknown kinds count as sistem.
func CategoryOf(kind string) Category {
	switch strings.ToLower(kind) {
	case "booking", "papan", "pembayaran", "ulasan":
		return CategoryBooking
	case "chat", "pesan":
		return CategoryChat
	case "sengketa":
		return CategorySengketa
	default:
		return CategorySistem
	}
}

// importantKinds bypass quiet hours. Disputes have deadlines (L1 48 hours); OTP and security notices are urgent.
var importantKinds = map[string]bool{"sengketa": true, "otp": true, "keamanan": true}

// Notification is what the core hands to Enqueue: the in-app notification it just inserted.
type Notification struct {
	ID     string // in-app notification id (notifications.id)
	UserID string
	Kind   string // booking | chat | sengketa | sistem | verifikasi | papan | otp | ...
	Title  string
	Body   string
	Href   string // app path, e.g. "/sewaan/b-123"; made absolute with PUBLIC_URL for email
	// Tag groups push notifications on the device (a newer one with the same tag replaces the older). Defaults to ID.
	Tag string
	// Important bypasses quiet hours. Kinds sengketa, otp and keamanan are always important.
	Important bool
}

func (n Notification) important() bool { return n.Important || importantKinds[strings.ToLower(n.Kind)] }

// Payload is stored in notification_outbox.payload and handed to senders.
type Payload struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Href      string   `json:"href,omitempty"` // app path as given by the core
	URL       string   `json:"url,omitempty"`  // absolute link (PUBLIC_URL + href)
	Tag       string   `json:"tag,omitempty"`
	Kind      string   `json:"kind"`
	Category  Category `json:"category"`
	Important bool     `json:"important,omitempty"`
	To        string   `json:"to,omitempty"` // email address snapshot for the email channel
}

// DB is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// permanentError marks failures that retrying cannot fix (bad address, subscription gone, payload rejected).
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the dispatcher marks the job dead instead of retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err (or anything it wraps) was marked with Permanent.
func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}
