package ws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"auction/internal/auth"
	"auction/internal/httpx"
)

// Client is one browser connection. All writes go through the buffered `send`
// channel and a single writer goroutine, so nothing else touches the socket.
type Client struct {
	id          string
	user        *auth.User
	hub         *Hub
	conn        *websocket.Conn
	send        chan []byte
	ctx         context.Context
	cancel      context.CancelFunc
	subs        map[string]struct{} // guarded by hub.mu
	connectedAt time.Time
	closeOnce   sync.Once
	closed      atomic.Bool
}

type clientMsg struct {
	Action    string `json:"action"` // subscribe | unsubscribe | ping
	AuctionID string `json:"auction_id"`
}

// ServeWS upgrades GET /ws. It must sit behind Authenticate + RequireAuth
// (the browser passes the Supabase token as ?token=).
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if u == nil {
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}
	// Accept enforces same-origin by default: fine, UI and API share one URL.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.Debug("websocket accept failed", "err", err)
		return
	}
	conn.SetReadLimit(4096)

	// Not tied to r.Context(): the connection outlives the HTTP handler's normal lifetime rules.
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		id: randomID(), user: u, hub: h, conn: conn,
		send: make(chan []byte, sendBuffer), ctx: ctx, cancel: cancel,
		subs: make(map[string]struct{}), connectedAt: time.Now(),
	}
	if !h.register(c) {
		_ = conn.Close(websocket.StatusTryAgainLater, "server restarting")
		cancel()
		return
	}
	defer func() {
		h.unregister(c)
		c.close(websocket.StatusNormalClosure, "")
		cancel()
	}()
	slog.Info("ws connected", "client_id", c.id, "user_id", u.ID)

	go c.writePump()
	c.sendJSON(map[string]any{"type": "hello", "client_id": c.id, "server_time": time.Now().UTC()})
	c.readPump()
	slog.Info("ws disconnected", "client_id", c.id, "user_id", u.ID)
}

func (c *Client) readPump() {
	windowStart, count := time.Now(), 0
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		// Simple flood guard: max 30 messages per 10 seconds.
		if time.Since(windowStart) > 10*time.Second {
			windowStart, count = time.Now(), 0
		}
		if count++; count > 30 {
			c.close(websocket.StatusPolicyViolation, "too many messages")
			return
		}

		var m clientMsg
		if err := json.Unmarshal(data, &m); err != nil {
			c.sendError("bad_request", "Invalid message.")
			continue
		}
		switch m.Action {
		case "subscribe":
			if !httpx.IsUUID(m.AuctionID) {
				c.sendError("bad_request", "Invalid auction id.")
				continue
			}
			if err := c.hub.subscribe(c, m.AuctionID); err != nil {
				c.sendError("subscribe_failed", "Could not subscribe.")
				continue
			}
			c.sendJSON(map[string]any{"type": "subscribed", "auction_id": m.AuctionID})
		case "unsubscribe":
			c.hub.unsubscribe(c, m.AuctionID)
			c.sendJSON(map[string]any{"type": "unsubscribed", "auction_id": m.AuctionID})
		case "ping":
			c.sendJSON(map[string]any{"type": "pong", "server_time": time.Now().UTC()})
		default:
			c.sendError("bad_request", "Unknown action.")
		}
	}
}

// writePump is the only goroutine that writes to the socket. It also sends a
// protocol ping every 25s; no pong within 10s means the peer is gone.
func (c *Client) writePump() {
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.send:
			wctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "write failed")
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
			err := c.conn.Ping(pctx) // needs readPump running to receive the pong
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}

// enqueue never blocks. If the client's buffer is full it is too slow: drop
// the CLIENT, not the message flow for everyone else.
func (c *Client) enqueue(msg []byte) {
	if c.closed.Load() {
		return
	}
	select {
	case c.send <- msg:
	default:
		c.hub.droppedSlow.Add(1)
		slog.Warn("dropping slow websocket client", "client_id", c.id, "user_id", c.user.ID)
		c.close(websocket.StatusPolicyViolation, "too slow")
	}
}

func (c *Client) sendJSON(v any) {
	if data, err := json.Marshal(v); err == nil {
		c.enqueue(data)
	}
}

func (c *Client) sendError(code, msg string) {
	c.sendJSON(map[string]any{"type": "error", "code": code, "message": msg})
}

// close sends a close frame (so the browser sees the reason), then cancels the
// context. Safe to call many times and from any goroutine.
func (c *Client) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		go func() {
			_ = c.conn.Close(code, reason)
			c.cancel()
		}()
	})
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
