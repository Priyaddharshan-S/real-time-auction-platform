// Package ws is the realtime layer. Every app instance runs one Hub:
//
//	bid committed -> Redis PUBLISH auction:{id} -> every instance's Hub receives it
//	-> Hub forwards it to the WebSocket clients on THAT instance subscribed to {id}.
//
// So it works the same with one Render instance or ten.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"
)

const (
	controlChannel   = "hub:control" // admin actions (kick) fan out to all instances
	auctionPrefix    = "auction:"
	maxSubsPerClient = 20
	sendBuffer       = 32 // per-client queue; a full queue means a slow client
)

var errTooManySubs = errors.New("too many subscriptions")

type Hub struct {
	rdb *redis.Client
	ps  *redis.PubSub

	// mu guards rooms, clients, closing and every Client.subs map.
	mu      sync.RWMutex
	rooms   map[string]map[*Client]struct{} // auction id -> local subscribers
	clients map[string]*Client              // client id -> client
	closing bool
	wg      sync.WaitGroup // one count per live connection

	// subMu serialises Redis SUBSCRIBE/UNSUBSCRIBE so the "first/last local
	// subscriber" decision and the Redis call can never interleave wrongly.
	subMu sync.Mutex

	droppedSlow atomic.Int64
	startedAt   time.Time
}

// NewHub subscribes to the control channel and waits for Redis to confirm,
// so a broken Redis connection fails at boot instead of silently later.
func NewHub(ctx context.Context, rdb *redis.Client) (*Hub, error) {
	ps := rdb.Subscribe(ctx, controlChannel)
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := ps.Receive(rctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("redis subscribe: %w", err)
	}
	return &Hub{
		rdb:       rdb,
		ps:        ps,
		rooms:     make(map[string]map[*Client]struct{}),
		clients:   make(map[string]*Client),
		startedAt: time.Now(),
	}, nil
}

// Start begins forwarding Redis messages. The health-check interval is long to
// keep Upstash command usage low (go-redis pings while idle).
func (h *Hub) Start() {
	ch := h.ps.Channel(redis.WithChannelHealthCheckInterval(time.Minute), redis.WithChannelSize(1024))
	go func() {
		for msg := range ch {
			h.dispatch(msg)
		}
	}()
}

func (h *Hub) dispatch(msg *redis.Message) {
	if msg.Channel == controlChannel {
		h.handleControl([]byte(msg.Payload))
		return
	}
	if id, ok := strings.CutPrefix(msg.Channel, auctionPrefix); ok {
		h.broadcast(id, []byte(msg.Payload))
	}
}

// broadcast copies the recipient list under the read lock, then sends with the
// lock released. Sends never block (see Client.enqueue), so one slow browser
// cannot delay anyone else.
func (h *Hub) broadcast(auctionID string, payload []byte) {
	h.mu.RLock()
	room := h.rooms[auctionID]
	targets := make([]*Client, 0, len(room))
	for c := range room {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.enqueue(payload)
	}
}

func (h *Hub) register(c *Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	h.clients[c.id] = c
	h.wg.Add(1)
	return true
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	ids := make([]string, 0, len(c.subs))
	for id := range c.subs {
		ids = append(ids, id)
	}
	delete(h.clients, c.id)
	h.mu.Unlock()
	for _, id := range ids {
		h.unsubscribe(c, id)
	}
	h.wg.Done()
}

func (h *Hub) subscribe(c *Client, auctionID string) error {
	h.subMu.Lock()
	defer h.subMu.Unlock()

	h.mu.Lock()
	if _, already := c.subs[auctionID]; already {
		h.mu.Unlock()
		return nil
	}
	if len(c.subs) >= maxSubsPerClient {
		h.mu.Unlock()
		return errTooManySubs
	}
	room := h.rooms[auctionID]
	if room == nil {
		room = make(map[*Client]struct{})
		h.rooms[auctionID] = room
	}
	room[c] = struct{}{}
	c.subs[auctionID] = struct{}{}
	first := len(room) == 1
	h.mu.Unlock()

	if first { // first local subscriber: start listening on Redis for this auction
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.ps.Subscribe(ctx, auctionPrefix+auctionID); err != nil {
			h.mu.Lock()
			h.removeLocked(c, auctionID)
			h.mu.Unlock()
			return err
		}
	}
	return nil
}

func (h *Hub) unsubscribe(c *Client, auctionID string) {
	h.subMu.Lock()
	defer h.subMu.Unlock()

	h.mu.Lock()
	if _, ok := c.subs[auctionID]; !ok {
		h.mu.Unlock()
		return
	}
	empty := h.removeLocked(c, auctionID)
	h.mu.Unlock()

	if empty { // last local subscriber left: stop listening on Redis
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.ps.Unsubscribe(ctx, auctionPrefix+auctionID); err != nil {
			slog.Warn("redis unsubscribe failed", "auction_id", auctionID, "err", err)
		}
	}
}

// removeLocked requires h.mu held for writing. Returns true if the room is now empty.
func (h *Hub) removeLocked(c *Client, auctionID string) bool {
	delete(c.subs, auctionID)
	room := h.rooms[auctionID]
	delete(room, c)
	if len(room) == 0 {
		delete(h.rooms, auctionID)
		return true
	}
	return false
}

// ---- admin controls (used by the admin API in Phase 5) ----

type control struct {
	Type     string `json:"type"` // kick_user | kick_client
	UserID   string `json:"user_id,omitempty"`
	ClientID string `json:"client_id,omitempty"`
}

// KickUser disconnects every socket of a user on ALL instances. Close code
// 1008 tells the browser not to auto-reconnect.
func (h *Hub) KickUser(ctx context.Context, userID string) int {
	n := h.kickWhere(func(c *Client) bool { return c.user.ID == userID }, "removed by admin")
	h.publishControl(ctx, control{Type: "kick_user", UserID: userID})
	return n
}

// KickClient disconnects one socket by id (on whichever instance holds it).
func (h *Hub) KickClient(ctx context.Context, clientID string) int {
	n := h.kickWhere(func(c *Client) bool { return c.id == clientID }, "removed by admin")
	h.publishControl(ctx, control{Type: "kick_client", ClientID: clientID})
	return n
}

func (h *Hub) handleControl(data []byte) {
	var m control
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case "kick_user":
		h.kickWhere(func(c *Client) bool { return c.user.ID == m.UserID }, "removed by admin")
	case "kick_client":
		h.kickWhere(func(c *Client) bool { return c.id == m.ClientID }, "removed by admin")
	}
}

func (h *Hub) kickWhere(match func(*Client) bool, reason string) int {
	h.mu.RLock()
	var targets []*Client
	for _, c := range h.clients {
		if match(c) {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.close(websocket.StatusPolicyViolation, reason)
	}
	return len(targets)
}

func (h *Hub) publishControl(ctx context.Context, m control) {
	data, _ := json.Marshal(m)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := h.rdb.Publish(pctx, controlChannel, data).Err(); err != nil {
		slog.Warn("publish control failed", "err", err)
	}
}

// ---- introspection (admin dashboard, /debug/stats) ----

type ClientInfo struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	ConnectedAt time.Time `json:"connected_at"`
	Auctions    []string  `json:"auctions"`
}

// Snapshot lists sockets connected to THIS instance.
func (h *Hub) Snapshot() []ClientInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]ClientInfo, 0, len(h.clients))
	for _, c := range h.clients {
		subs := make([]string, 0, len(c.subs))
		for id := range c.subs {
			subs = append(subs, id)
		}
		out = append(out, ClientInfo{ID: c.id, UserID: c.user.ID, Email: c.user.Email,
			DisplayName: c.user.DisplayName, ConnectedAt: c.connectedAt, Auctions: subs})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out
}

type Stats struct {
	Clients       int            `json:"clients"`
	Rooms         map[string]int `json:"rooms"` // auction id -> subscribers
	DroppedSlow   int64          `json:"dropped_slow_clients"`
	UptimeSeconds int64          `json:"uptime_seconds"`
}

func (h *Hub) Stats() Stats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	rooms := make(map[string]int, len(h.rooms))
	for id, r := range h.rooms {
		rooms[id] = len(r)
	}
	return Stats{Clients: len(h.clients), Rooms: rooms, DroppedSlow: h.droppedSlow.Load(),
		UptimeSeconds: int64(time.Since(h.startedAt).Seconds())}
}

// Shutdown: refuse new sockets, tell every client to go away (1001 = reconnect
// elsewhere), wait for the connection goroutines to drain, then close Redis.
func (h *Hub) Shutdown(ctx context.Context) {
	h.mu.Lock()
	h.closing = true
	targets := make([]*Client, 0, len(h.clients))
	for _, c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.Unlock()

	for _, c := range targets {
		c.close(websocket.StatusGoingAway, "server restarting")
	}
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		slog.Warn("websocket drain timed out")
	}
	_ = h.ps.Close()
}
