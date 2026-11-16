// Package realtime pushes "dataset changed" signals to connected web clients over WebSocket.
//
// The core produces one Event per committed transaction. The Hub delivers {"type":"changed","version":n} only to
// the connections the event concerns: the listed users, every admin when Admins is set, and every connection
// (guests included) when Public is set. Clients then refetch their scoped snapshot (GET /api/v1/state).
//
// Across instances, events travel through Postgres LISTEN/NOTIFY (see Publisher and Listener).
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Event is what the core emits after every committed transaction.
type Event struct {
	Version int64    `json:"version"`
	Users   []string `json:"users,omitempty"`  // users whose scoped snapshot changed
	Admins  bool     `json:"admins,omitempty"` // every admin should refetch
	Public  bool     `json:"public,omitempty"` // public data changed: everyone, guests included, should refetch
}

// Resolver identifies the viewer of a WebSocket request. An empty userID means a guest. role "admin" marks admins.
type Resolver func(r *http.Request) (userID, role string)

// DefaultMaxGuestConns is the guest connection cap when Options.MaxGuestConns is 0. Every connection holds
// goroutines and buffers, so an uncapped hub lets one client exhaust the process' memory.
const DefaultMaxGuestConns = 1000

// RoleAdmin is the role string that subscribes a connection to Admins events.
const RoleAdmin = "admin"

// Options configures a Hub. Zero values pick the defaults noted on each field.
type Options struct {
	// Resolve authenticates the connection. Nil treats every connection as a guest.
	Resolve Resolver
	// AllowedOrigins lists browser origins allowed to connect, e.g. "https://sewain.hallojanu.me". Entries may be
	// path.Match patterns ("https://*.hallojanu.me") and "*" allows any origin. Same-host origins and requests
	// without an Origin header (non-browser clients) are always allowed.
	AllowedOrigins []string
	// CurrentVersion returns the dataset version sent in the hello message. Nil uses the highest published version.
	CurrentVersion func(ctx context.Context) (int64, error)
	// MaxConnsPerUser caps simultaneous connections of one signed-in user (default 5). Extra ones get HTTP 429.
	MaxConnsPerUser int
	// MaxGuestConns caps guest connections on this instance. 0 means DefaultMaxGuestConns; a negative value
	// means unlimited (only behind another connection limit).
	MaxGuestConns int
	// SendBuffer is the per-connection queue length (default 16). A connection whose queue is full is dropped.
	SendBuffer int
	// WriteTimeout bounds one write to a client (default 5s). A slower client is dropped.
	WriteTimeout time.Duration
	// PingInterval is the keepalive period (default 25s, below common proxy idle timeouts).
	PingInterval time.Duration
	// PingTimeout bounds the wait for a pong (default 10s).
	PingTimeout time.Duration
	Logger      *slog.Logger
}

// Hub tracks local WebSocket connections and routes events to them. It never blocks on a client.
type Hub struct {
	opt Options
	log *slog.Logger

	mu      sync.Mutex
	clients map[*client]struct{}
	perUser map[string]int
	guests  int
	version int64
	closed  bool
}

type client struct {
	userID string
	admin  bool
	send   chan []byte
	cancel context.CancelFunc
	once   sync.Once
}

func NewHub(opt Options) *Hub {
	if opt.MaxConnsPerUser <= 0 {
		opt.MaxConnsPerUser = 5
	}
	if opt.MaxGuestConns == 0 {
		opt.MaxGuestConns = DefaultMaxGuestConns
	}
	if opt.SendBuffer <= 0 {
		opt.SendBuffer = 16
	}
	if opt.WriteTimeout <= 0 {
		opt.WriteTimeout = 5 * time.Second
	}
	if opt.PingInterval <= 0 {
		opt.PingInterval = 25 * time.Second
	}
	if opt.PingTimeout <= 0 {
		opt.PingTimeout = 10 * time.Second
	}
	log := opt.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Hub{opt: opt, log: log, clients: map[*client]struct{}{}, perUser: map[string]int{}}
}

type message struct {
	Type    string `json:"type"`
	Version int64  `json:"version"`
}

func encode(typ string, v int64) []byte {
	b, _ := json.Marshal(message{Type: typ, Version: v})
	return b
}

// Publish delivers e to the affected local connections. It never blocks: a connection whose queue is full is
// dropped (the browser reconnects and refetches, so nothing is lost).
func (h *Hub) Publish(e Event) {
	msg := encode("changed", e.Version)
	users := make(map[string]struct{}, len(e.Users))
	for _, u := range e.Users {
		users[u] = struct{}{}
	}
	var slow []*client
	h.mu.Lock()
	if e.Version > h.version {
		h.version = e.Version
	}
	for c := range h.clients {
		_, mine := users[c.userID]
		if !e.Public && !(e.Admins && c.admin) && !(c.userID != "" && mine) {
			continue
		}
		select {
		case c.send <- msg:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.Unlock()
	for _, c := range slow {
		h.log.Warn("realtime: dropping slow client", "user", c.userID)
		c.stop()
	}
}

// Count returns the number of live connections on this instance.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Close disconnects every client and refuses new ones.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	cs := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		cs = append(cs, c)
	}
	h.mu.Unlock()
	for _, c := range cs {
		c.stop()
	}
}

func (c *client) stop() { c.once.Do(c.cancel) }

var errOrigin = errors.New("origin not allowed")

func (h *Hub) checkOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return errOrigin
	}
	if strings.EqualFold(u.Host, r.Host) {
		return nil
	}
	full := strings.ToLower(u.Scheme + "://" + u.Host)
	for _, p := range h.opt.AllowedOrigins {
		p = strings.ToLower(strings.TrimRight(strings.TrimSpace(p), "/"))
		if p == "*" || p == full {
			return nil
		}
		if ok, _ := path.Match(p, full); ok {
			return nil
		}
	}
	return errOrigin
}

func (h *Hub) register(userID, role string) (*client, context.Context, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, http.StatusServiceUnavailable
	}
	if userID == "" {
		if h.opt.MaxGuestConns > 0 && h.guests >= h.opt.MaxGuestConns {
			return nil, nil, http.StatusTooManyRequests
		}
		h.guests++
	} else {
		if h.perUser[userID] >= h.opt.MaxConnsPerUser {
			return nil, nil, http.StatusTooManyRequests
		}
		h.perUser[userID]++
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{userID: userID, admin: role == RoleAdmin, send: make(chan []byte, h.opt.SendBuffer), cancel: cancel}
	h.clients[c] = struct{}{}
	return c, ctx, 0
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		if c.userID == "" {
			h.guests--
		} else if h.perUser[c.userID]--; h.perUser[c.userID] <= 0 {
			delete(h.perUser, c.userID)
		}
	}
	h.mu.Unlock()
	c.stop()
}

func (h *Hub) helloVersion(ctx context.Context) int64 {
	h.mu.Lock()
	v := h.version
	h.mu.Unlock()
	if h.opt.CurrentVersion != nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if cur, err := h.opt.CurrentVersion(ctx); err == nil && cur > v {
			v = cur
		} else if err != nil {
			h.log.Warn("realtime: current version", "err", err)
		}
	}
	return v
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
}

// ServeHTTP upgrades GET /api/v1/events and holds the connection until either side leaves.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.checkOrigin(r); err != nil {
		writeErr(w, http.StatusForbidden, "Asal permintaan tidak diizinkan.")
		return
	}
	var userID, role string
	if h.opt.Resolve != nil {
		userID, role = h.opt.Resolve(r)
	}
	c, ctx, status := h.register(userID, role)
	if c == nil {
		if status == http.StatusTooManyRequests {
			writeErr(w, status, "Terlalu banyak tab yang terhubung. Tutup sebagian, ya.")
		} else {
			writeErr(w, status, "Server sedang berhenti. Coba lagi sebentar lagi, ya.")
		}
		return
	}
	defer h.unregister(c)

	// Origin was checked above against our own list, so the library check is skipped.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1024) // clients never need to send anything meaningful

	// Read loop: required for pong/close handling. Any client message is ignored; a read error ends the session.
	go func() {
		defer c.stop()
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	hello := encode("hello", h.helloVersion(r.Context()))
	if !h.write(ctx, conn, hello) {
		return
	}
	ping := time.NewTicker(h.opt.PingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusGoingAway, "")
			return
		case msg := <-c.send:
			if !h.write(ctx, conn, msg) {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, h.opt.PingTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (h *Hub) write(ctx context.Context, conn *websocket.Conn, msg []byte) bool {
	wctx, cancel := context.WithTimeout(ctx, h.opt.WriteTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, msg) == nil
}
