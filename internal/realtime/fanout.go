package realtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Channel is the Postgres NOTIFY channel shared by every API instance.
const Channel = "sewain_events"

// MaxPayload keeps the NOTIFY payload under Postgres' 8000-byte limit with some headroom.
const MaxPayload = 7900

// ListenerAppName is the application_name of listener connections (visible in pg_stat_activity).
const ListenerAppName = "sewain-listener"

// Execer is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// envelope is the NOTIFY payload. Origin lets the sending instance skip its own echo.
type envelope struct {
	Origin string `json:"o,omitempty"`
	Event  Event  `json:"e"`
}

// NewInstanceID returns a random id that tells this process' notifications apart from other instances'.
func NewInstanceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// encodeEnvelope marshals the payload and falls back to a compact Public event when the user list is too long
// for NOTIFY: every client then refetches, which is wasteful but correct.
func encodeEnvelope(origin string, e Event) []byte {
	b, _ := json.Marshal(envelope{Origin: origin, Event: e})
	if len(b) <= MaxPayload {
		return b
	}
	b, _ = json.Marshal(envelope{Origin: origin, Event: Event{Version: e.Version, Public: true}})
	return b
}

// Publisher sends events to every instance: straight to the local hub and through pg_notify to the others.
type Publisher struct {
	DB     Execer // usually the *pgxpool.Pool
	Hub    *Hub   // local hub; nil when this process serves no WebSockets
	Origin string // this instance's id, shared with its Listener
}

// Publish delivers e locally right away and to other instances via NOTIFY. Call it after the transaction commits.
func (p *Publisher) Publish(ctx context.Context, e Event) error {
	if p.Hub != nil {
		p.Hub.Publish(e)
	}
	return notify(ctx, p.DB, encodeEnvelope(p.Origin, e))
}

// PublishTx queues e inside tx. Postgres only delivers it if tx commits, so a rolled back action never signals
// clients. Every instance, this one included, receives it through its Listener.
func (p *Publisher) PublishTx(ctx context.Context, tx pgx.Tx, e Event) error {
	return notify(ctx, tx, encodeEnvelope("", e))
}

func notify(ctx context.Context, db Execer, payload []byte) error {
	_, err := db.Exec(ctx, "SELECT pg_notify($1, $2)", Channel, string(payload))
	return err
}

// Listener holds a dedicated connection on LISTEN sewain_events and forwards foreign events to the local hub.
type Listener struct {
	ConnString string // DATABASE_URL; a dedicated connection, not from the pool
	Hub        *Hub
	Origin     string // same id as the Publisher of this instance
	Logger     *slog.Logger
	// MaxBackoff caps the reconnect delay (default 30s).
	MaxBackoff time.Duration
	// OnListen, if set, runs every time LISTEN is (re)established. Useful for readiness and tests.
	OnListen func()
}

// Run listens until ctx ends, reconnecting with exponential backoff. After a reconnect it publishes a Public
// event locally so clients refetch whatever they may have missed while the connection was down.
func (l *Listener) Run(ctx context.Context) error {
	log := l.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	maxBackoff := l.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	backoff := 500 * time.Millisecond
	var lastVersion int64
	connected := false
	for {
		err := l.session(ctx, connected, &lastVersion, func() {
			connected = true
			backoff = 500 * time.Millisecond
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Warn("realtime: listener disconnected, retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (l *Listener) session(ctx context.Context, reconnect bool, lastVersion *int64, ok func()) error {
	cfg, err := pgx.ParseConfig(l.ConnString)
	if err != nil {
		return err
	}
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = ListenerAppName
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return err
	}
	ok()
	if reconnect && l.Hub != nil {
		l.Hub.Publish(Event{Version: *lastVersion, Public: true})
	}
	if l.OnListen != nil {
		l.OnListen()
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal([]byte(n.Payload), &env); err != nil {
			continue
		}
		if env.Event.Version > *lastVersion {
			*lastVersion = env.Event.Version
		}
		if env.Origin != "" && env.Origin == l.Origin {
			continue // already delivered locally by Publisher.Publish
		}
		if l.Hub != nil {
			l.Hub.Publish(env.Event)
		}
	}
}
