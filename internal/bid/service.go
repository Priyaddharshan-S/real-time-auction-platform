// Package bid implements placing bids. The database decides the winner.
package bid

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"auction/internal/auction"
	"auction/internal/httpx"
)

const (
	maxAmount  = 1_000_000_000_000
	rateLimit  = 8 // max bids per user per window
	rateWindow = 10 * time.Second
)

// placeTimeout bounds one bid. A var (not const) so the 100-way stress test can
// allow for a slow remote database.
var placeTimeout = 5 * time.Second

type PlaceInput struct {
	AuctionID      string
	UserID         string
	DisplayName    string
	Amount         int64
	IdempotencyKey string
}

type Result struct {
	Auction  *auction.Auction `json:"auction"`
	Bid      *auction.Bid     `json:"bid"`
	Replayed bool             `json:"replayed"` // true when this was a retry of an earlier bid
}

type Service struct{ as *auction.Store }

func NewService(as *auction.Store) *Service { return &Service{as: as} }

// placeSQL is the heart of the system: ONE atomic statement. Postgres locks
// the auction row, and if another bid commits first, the WHERE clause is
// re-checked against the new price. So under 100 concurrent bids, only bids
// that still beat the current price + increment match a row. Zero rows = reject.
// Anti-sniping: a bid in the last 30s pushes ends_at out by 30s, in this same statement.
const placeSQL = `WITH u AS (
	UPDATE auctions SET
		current_price = $1,
		leader_id     = $2,
		bid_count     = bid_count + 1,
		ends_at       = CASE WHEN ends_at - now() <= interval '30 seconds'
		                     THEN ends_at + interval '30 seconds' ELSE ends_at END
	WHERE id = $3
	  AND status = 'active'
	  AND ends_at > now()
	  AND current_price + min_increment <= $1
	RETURNING *)
SELECT ` + auction.SelectCols + auction.FromCTE

// Place validates, rate-limits, then runs the bid transaction.
func (s *Service) Place(ctx context.Context, in PlaceInput) (*Result, error) {
	if in.Amount <= 0 || in.Amount > maxAmount || len(in.IdempotencyKey) < 8 || len(in.IdempotencyKey) > 64 {
		return nil, httpx.ErrBadRequest
	}
	if err := s.checkRate(ctx, in.UserID); err != nil {
		return nil, err
	}
	return s.place(ctx, in)
}

func (s *Service) place(ctx context.Context, in PlaceInput) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, placeTimeout)
	defer cancel()

	// Retry / double-tap: if this key was already processed, return that result.
	// (Done on the pool BEFORE opening a transaction, so no connection is held twice.)
	if res, err := s.replay(ctx, s.as.DB, in); err != nil || res != nil {
		return res, err
	}

	tx, err := s.as.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	a, err := auction.ScanAuction(tx.QueryRow(ctx, placeSQL, in.Amount, in.UserID, in.AuctionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return s.explain(ctx, tx, in) // zero rows: work out WHY and return a typed error
	}
	if err != nil {
		return nil, err
	}

	b := &auction.Bid{AuctionID: in.AuctionID, UserID: in.UserID, DisplayName: in.DisplayName, Amount: in.Amount}
	err = tx.QueryRow(ctx, `INSERT INTO bids (auction_id, user_id, amount, idempotency_key)
		VALUES ($1,$2,$3,$4) RETURNING id, created_at`,
		in.AuctionID, in.UserID, in.Amount, in.IdempotencyKey).Scan(&b.ID, &b.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // same key arrived twice at once
			_ = tx.Rollback(ctx)
			if res, rerr := s.replay(ctx, s.as.DB, in); rerr != nil || res != nil {
				return res, rerr
			}
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// Only after commit: tell every instance (and thus every browser).
	s.as.Publish(ctx, auction.Event{Type: "bid", AuctionID: in.AuctionID, Auction: a, Bid: b})
	return &Result{Auction: a, Bid: b}, nil
}

// replay returns the stored result for an idempotency key, or (nil, nil) if new.
func (s *Service) replay(ctx context.Context, q auction.Querier, in PlaceInput) (*Result, error) {
	var b auction.Bid
	err := q.QueryRow(ctx, `SELECT b.id, b.auction_id, b.user_id, p.display_name, b.amount, b.created_at
		FROM bids b JOIN profiles p ON p.user_id = b.user_id
		WHERE b.auction_id=$1 AND b.idempotency_key=$2`, in.AuctionID, in.IdempotencyKey).
		Scan(&b.ID, &b.AuctionID, &b.UserID, &b.DisplayName, &b.Amount, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b.UserID != in.UserID {
		return nil, httpx.ErrBadRequest // someone else's key
	}
	a, err := auction.GetWith(ctx, q, in.AuctionID)
	if err != nil {
		return nil, err
	}
	return &Result{Auction: a, Bid: &b, Replayed: true}, nil
}

// explain turns "zero rows updated" into a precise typed error.
func (s *Service) explain(ctx context.Context, tx pgx.Tx, in PlaceInput) (*Result, error) {
	// A concurrent duplicate may have just won; that is a success, not an error.
	if res, err := s.replay(ctx, tx, in); err != nil || res != nil {
		return res, err
	}
	var status string
	var ended bool
	var minNext int64
	err := tx.QueryRow(ctx, `SELECT status, ends_at <= now(), current_price + min_increment
		FROM auctions WHERE id=$1`, in.AuctionID).Scan(&status, &ended, &minNext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, httpx.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "active" || ended {
		return nil, httpx.ErrAuctionClosed
	}
	return nil, httpx.ErrBidTooLow.With("min_next_bid", minNext)
}

// Fixed-window per-user limiter in Redis. The Lua script makes INCR + EXPIRE atomic.
var rateScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
return n`)

func (s *Service) checkRate(ctx context.Context, userID string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := rateScript.Run(ctx, s.as.Redis, []string{"rl:bid:" + userID}, rateWindow.Milliseconds()).Int()
	if err != nil {
		// Fail open: a Redis outage should not stop bidding (the DB still guards correctness).
		slog.Warn("rate limit check failed, allowing bid", "err", err)
		return nil
	}
	if n > rateLimit {
		return httpx.ErrRateLimited.With("retry_after_seconds", int(rateWindow.Seconds()))
	}
	return nil
}
