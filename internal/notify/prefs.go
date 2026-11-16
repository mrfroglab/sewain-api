package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// QuietHours defers non-important notifications while the user sleeps. Times are "HH:MM" in Timezone.
// A window may wrap midnight (22:00–06:00). Start == End means no quiet window.
type QuietHours struct {
	Enabled  bool   `json:"enabled"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Timezone string `json:"timezone"`
}

// Prefs is a user's notification preferences. Channels[inapp] is always true and cannot be changed.
type Prefs struct {
	Channels   map[Channel]map[Category]bool `json:"channels"`
	QuietHours QuietHours                    `json:"quietHours"`
}

// DefaultPrefs: in-app always, push for everything (only delivered when the user subscribed), email only for
// sengketa and sistem, WhatsApp off. Quiet hours 22.00–06.00 WIB.
func DefaultPrefs() Prefs {
	p := Prefs{Channels: map[Channel]map[Category]bool{}, QuietHours: QuietHours{Enabled: true, Start: "22:00", End: "06:00", Timezone: "Asia/Jakarta"}}
	for _, ch := range append([]Channel{ChannelInApp}, QueuedChannels...) {
		p.Channels[ch] = map[Category]bool{}
		for _, c := range Categories {
			switch ch {
			case ChannelInApp, ChannelWebPush:
				p.Channels[ch][c] = true
			case ChannelEmail:
				p.Channels[ch][c] = c == CategorySengketa || c == CategorySistem
			default:
				p.Channels[ch][c] = false
			}
		}
	}
	return p
}

// Enabled reports whether the user wants category c on channel ch.
func (p Prefs) Enabled(ch Channel, c Category) bool {
	if ch == ChannelInApp {
		return true
	}
	return p.Channels[ch][c]
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// QuietUntil returns the end of the quiet window containing now, or the zero time when now is outside quiet hours.
func (q QuietHours) QuietUntil(now time.Time) time.Time {
	if !q.Enabled {
		return time.Time{}
	}
	start, err1 := parseClock(q.Start)
	end, err2 := parseClock(q.End)
	loc, err3 := time.LoadLocation(q.Timezone)
	if err1 != nil || err2 != nil || err3 != nil || start == end {
		return time.Time{}
	}
	local := now.In(loc)
	m := local.Hour()*60 + local.Minute()
	inside := (start < end && m >= start && m < end) || (start > end && (m >= start || m < end))
	if !inside {
		return time.Time{}
	}
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	until := day.Add(time.Duration(end) * time.Minute)
	if !until.After(local) {
		until = until.AddDate(0, 0, 1)
	}
	return until
}

// Validate checks the quiet hours fields.
func (q QuietHours) Validate() error {
	if _, err := parseClock(q.Start); err != nil {
		return errBadClock
	}
	if _, err := parseClock(q.End); err != nil {
		return errBadClock
	}
	if _, err := time.LoadLocation(q.Timezone); err != nil || q.Timezone == "" || q.Timezone == "Local" {
		return errBadTimezone
	}
	return nil
}

// UserError carries a message meant for the user (Bahasa Indonesia); handlers return it as a 400.
type UserError string

func (e UserError) Error() string { return string(e) }

const (
	errBadClock    UserError = "Format jam harus JJ:MM, misalnya 22:00."
	errBadTimezone UserError = "Zona waktu tidak dikenal. Pakai misalnya Asia/Jakarta."
	errUnknownPref UserError = "Ada channel atau jenis notifikasi yang tidak dikenal."
)

const prefsColumns = `webpush_booking, webpush_chat, webpush_sengketa, webpush_sistem,
	email_booking, email_chat, email_sengketa, email_sistem,
	whatsapp_booking, whatsapp_chat, whatsapp_sengketa, whatsapp_sistem,
	quiet_enabled, to_char(quiet_start, 'HH24:MI'), to_char(quiet_end, 'HH24:MI'), timezone`

func (p *Prefs) scanTargets() []any {
	var t []any
	for _, ch := range QueuedChannels {
		for _, c := range Categories {
			v := p.Channels[ch][c]
			ptr := new(bool)
			*ptr = v
			t = append(t, ptr)
		}
	}
	return append(t, &p.QuietHours.Enabled, &p.QuietHours.Start, &p.QuietHours.End, &p.QuietHours.Timezone)
}

func (p *Prefs) applyScanned(t []any) {
	i := 0
	for _, ch := range QueuedChannels {
		for _, c := range Categories {
			p.Channels[ch][c] = *(t[i].(*bool))
			i++
		}
	}
}

// LoadPrefs returns the stored preferences of userID, or DefaultPrefs when the user never changed them.
func LoadPrefs(ctx context.Context, db DB, userID string) (Prefs, error) {
	p := DefaultPrefs()
	t := p.scanTargets()
	err := db.QueryRow(ctx, `SELECT `+prefsColumns+` FROM notification_prefs WHERE user_id = $1`, userID).Scan(t...)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultPrefs(), nil
	}
	if err != nil {
		return p, fmt.Errorf("load prefs: %w", err)
	}
	p.applyScanned(t)
	return p, nil
}

// SavePrefs upserts every preference of userID. In-app values are ignored (always on).
func SavePrefs(ctx context.Context, db DB, userID string, p Prefs) error {
	if err := p.QuietHours.Validate(); err != nil {
		return err
	}
	args := []any{userID}
	for _, ch := range QueuedChannels {
		for _, c := range Categories {
			args = append(args, p.Enabled(ch, c))
		}
	}
	args = append(args, p.QuietHours.Enabled, p.QuietHours.Start, p.QuietHours.End, p.QuietHours.Timezone)
	_, err := db.Exec(ctx, `
		INSERT INTO notification_prefs (user_id,
			webpush_booking, webpush_chat, webpush_sengketa, webpush_sistem,
			email_booking, email_chat, email_sengketa, email_sistem,
			whatsapp_booking, whatsapp_chat, whatsapp_sengketa, whatsapp_sistem,
			quiet_enabled, quiet_start, quiet_end, timezone)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::time, $16::time, $17)
		ON CONFLICT (user_id) DO UPDATE SET
			webpush_booking = excluded.webpush_booking, webpush_chat = excluded.webpush_chat,
			webpush_sengketa = excluded.webpush_sengketa, webpush_sistem = excluded.webpush_sistem,
			email_booking = excluded.email_booking, email_chat = excluded.email_chat,
			email_sengketa = excluded.email_sengketa, email_sistem = excluded.email_sistem,
			whatsapp_booking = excluded.whatsapp_booking, whatsapp_chat = excluded.whatsapp_chat,
			whatsapp_sengketa = excluded.whatsapp_sengketa, whatsapp_sistem = excluded.whatsapp_sistem,
			quiet_enabled = excluded.quiet_enabled, quiet_start = excluded.quiet_start,
			quiet_end = excluded.quiet_end, timezone = excluded.timezone, updated_at = now()`, args...)
	if err != nil {
		return fmt.Errorf("save prefs: %w", err)
	}
	return nil
}

// PrefsPatch is the body of PUT /api/v1/me/notification-prefs. Every field is optional.
type PrefsPatch struct {
	Channels   map[Channel]map[Category]*bool `json:"channels"`
	QuietHours *struct {
		Enabled  *bool   `json:"enabled"`
		Start    *string `json:"start"`
		End      *string `json:"end"`
		Timezone *string `json:"timezone"`
	} `json:"quietHours"`
}

// Apply merges the patch into p. In-app entries are accepted but ignored.
func (patch PrefsPatch) Apply(p *Prefs) error {
	for ch, cats := range patch.Channels {
		if ch == ChannelInApp {
			continue
		}
		if _, ok := p.Channels[ch]; !ok {
			return errUnknownPref
		}
		for c, v := range cats {
			if _, ok := p.Channels[ch][c]; !ok {
				return errUnknownPref
			}
			if v != nil {
				p.Channels[ch][c] = *v
			}
		}
	}
	if q := patch.QuietHours; q != nil {
		if q.Enabled != nil {
			p.QuietHours.Enabled = *q.Enabled
		}
		if q.Start != nil {
			p.QuietHours.Start = *q.Start
		}
		if q.End != nil {
			p.QuietHours.End = *q.End
		}
		if q.Timezone != nil {
			p.QuietHours.Timezone = *q.Timezone
		}
	}
	return p.QuietHours.Validate()
}
