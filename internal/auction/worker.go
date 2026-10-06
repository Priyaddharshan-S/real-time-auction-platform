package auction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const closerLockKey = "lock:auction-worker"

// Only delete the lock if we still own it (compare token, then DEL).
var unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end`)

// Close ends an auction. The status guard in the WHERE clause makes it atomic:
// if two callers race, exactly one UPDATE matches a row. force=false only closes
// auctions whose end time has passed (the worker); force=true is for admin
// "close now". Returns (nil, nil) when someone else already closed it.
func (s *Store) Close(ctx context.Context, id string, force bool) (*Auction, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, err := ScanAuction(s.DB.QueryRow(ctx, `WITH u AS (
		UPDATE auctions SET status='closed', winner_id=leader_id, closed_at=now()
		WHERE id=$1 AND status='active' AND (ends_at <= now() OR $2::boolean)
		RETURNING *) SELECT `+SelectCols+FromCTE, id, force))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	slog.Info("auction closed", "auction_id", id, "winner_id", a.WinnerID, "price", a.CurrentPrice)
	s.Publish(ctx, Event{Type: "auction_closed", AuctionID: id, Auction: a})
	return a, nil
}

// RunWorker starts scheduled auctions and closes expired ones until ctx ends.
func (s *Store) RunWorker(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Store) tick(parent context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("worker panic", "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()

	// Cheap DB check first, so an idle system sends nothing to Redis
	// (keeps Upstash command usage low).
	var due int
	err := s.DB.QueryRow(ctx, `SELECT count(*) FROM auctions
		WHERE (status='active' AND ends_at <= now()) OR (status='scheduled' AND starts_at <= now())`).Scan(&due)
	if err != nil {
		if parent.Err() == nil {
			slog.Warn("worker: count due", "err", err)
		}
		return
	}
	if due == 0 {
		return
	}

	// Redis lock: only one Render instance processes this batch. Even without
	// it the per-row status guard keeps results correct; the lock just avoids
	// duplicate work.
	token := randomToken()
	ok, err := s.Redis.SetNX(ctx, closerLockKey, token, 15*time.Second).Result()
	if err != nil {
		slog.Warn("worker: lock", "err", err)
		return
	}
	if !ok {
		return
	}
	defer func() {
		uctx, ucancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer ucancel()
		_ = unlockScript.Run(uctx, s.Redis, []string{closerLockKey}, token).Err()
	}()

	for _, id := range s.collectIDs(ctx, `UPDATE auctions SET status='active'
		WHERE status='scheduled' AND starts_at <= now() RETURNING id`) {
		s.PublishState(ctx, id, "auction_started")
	}
	for _, id := range s.collectIDs(ctx, `SELECT id FROM auctions
		WHERE status='active' AND ends_at <= now() LIMIT 100`) {
		if _, err := s.Close(ctx, id, false); err != nil {
			slog.Error("worker: close", "auction_id", id, "err", err)
		}
	}
}

// collectIDs runs a statement returning ids and reads them all before any other
// query runs (so the connection is released first).
func (s *Store) collectIDs(ctx context.Context, sql string) []string {
	rows, err := s.DB.Query(ctx, sql)
	if err != nil {
		slog.Warn("worker: query", "err", err)
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
