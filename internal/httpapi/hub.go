package httpapi

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Hub fans out "dataset changed" events to every connected client (PRD §13: WebSocket for chat & notifications).
// Clients refetch the snapshot on each event, so every device converges on the server state.
type Hub struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]context.CancelFunc
}

func NewHub() *Hub { return &Hub{conns: map[*websocket.Conn]context.CancelFunc{}} }

func (h *Hub) Broadcast(msg []byte) {
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := c.Write(ctx, websocket.MessageText, msg); err != nil {
			h.drop(c)
		}
		cancel()
	}
}

func (h *Hub) drop(c *websocket.Conn) {
	h.mu.Lock()
	if cancel, ok := h.conns[c]; ok {
		cancel()
		delete(h.conns, c)
	}
	h.mu.Unlock()
	_ = c.CloseNow()
}

func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// Serve upgrades the request and keeps the connection until the client leaves.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, origins []string, hello []byte) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: origins})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.conns[c] = cancel
	h.mu.Unlock()
	defer h.drop(c)
	_ = c.Write(ctx, websocket.MessageText, hello)
	// Read loop (we ignore client messages) + keepalive pings so proxies don't drop idle sockets.
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pc := context.WithTimeout(ctx, 10*time.Second)
				err := c.Ping(pctx)
				pc()
				if err != nil {
					h.drop(c)
					return
				}
			}
		}
	}()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}
