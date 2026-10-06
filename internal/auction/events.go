package auction

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Event is what gets published to Redis channel "auction:{id}". Every app
// instance subscribes (Phase 4) and forwards it to its own WebSocket clients.
type Event struct {
	Type      string   `json:"type"` // bid, auction_started, auction_closed, auction_updated, ...
	AuctionID string   `json:"auction_id"`
	Auction   *Auction `json:"auction,omitempty"`
	Bid       *Bid     `json:"bid,omitempty"`
}

func Channel(auctionID string) string { return "auction:" + auctionID }

// Publish is best-effort: the DB is the source of truth, so a failed publish is
// logged but never fails the request. It ignores caller cancellation so a
// committed bid is still announced if the client disconnects.
func (s *Store) Publish(ctx context.Context, ev Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		slog.Error("marshal event", "err", err)
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := s.Redis.Publish(pctx, Channel(ev.AuctionID), data).Err(); err != nil {
		slog.Warn("publish failed", "auction_id", ev.AuctionID, "err", err)
	}
}

// PublishState loads the current auction row and publishes it.
func (s *Store) PublishState(ctx context.Context, auctionID, eventType string) {
	a, err := s.Get(ctx, auctionID)
	if err != nil {
		slog.Warn("publish state: load auction", "auction_id", auctionID, "err", err)
		return
	}
	s.Publish(ctx, Event{Type: eventType, AuctionID: auctionID, Auction: a})
}
