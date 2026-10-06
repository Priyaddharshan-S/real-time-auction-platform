// Package auction holds the auction model, queries, events and the closer worker.
package auction

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Auction struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	ImageURL     string     `json:"image_url"`
	StartPrice   int64      `json:"start_price"`
	CurrentPrice int64      `json:"current_price"`
	MinIncrement int64      `json:"min_increment"`
	LeaderID     *string    `json:"leader_id"`
	LeaderName   *string    `json:"leader_name"`
	WinnerID     *string    `json:"winner_id"`
	BidCount     int        `json:"bid_count"`
	Status       string     `json:"status"`
	StartsAt     time.Time  `json:"starts_at"`
	EndsAt       time.Time  `json:"ends_at"`
	ClosedAt     *time.Time `json:"closed_at"`
}

type Bid struct {
	ID          string    `json:"id"`
	AuctionID   string    `json:"auction_id"`
	UserID      string    `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Amount      int64     `json:"amount"`
	CreatedAt   time.Time `json:"created_at"`
}

// Querier is satisfied by both *pgxpool.Pool and pgx.Tx. Code that runs inside
// a transaction must use the Tx, never the pool: holding a connection while
// asking the pool for another can deadlock under load.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	DB    *pgxpool.Pool
	Redis *redis.Client
}

func NewStore(db *pgxpool.Pool, rdb *redis.Client) *Store { return &Store{DB: db, Redis: rdb} }

// SelectCols / FromCTE let other packages build "WITH u AS (UPDATE ... RETURNING *)"
// statements that return a fully populated Auction (including leader name).
const (
	SelectCols = `a.id, a.title, a.description, a.image_url, a.start_price, a.current_price,
		a.min_increment, a.leader_id, p.display_name, a.winner_id, a.bid_count, a.status,
		a.starts_at, a.ends_at, a.closed_at`
	FromCTE  = ` FROM u a LEFT JOIN profiles p ON p.user_id = a.leader_id`
	fromJoin = ` FROM auctions a LEFT JOIN profiles p ON p.user_id = a.leader_id`
)

type scanner interface{ Scan(dest ...any) error }

// ScanAuction scans one row selected with SelectCols.
func ScanAuction(row scanner) (*Auction, error) {
	var a Auction
	err := row.Scan(&a.ID, &a.Title, &a.Description, &a.ImageURL, &a.StartPrice, &a.CurrentPrice,
		&a.MinIncrement, &a.LeaderID, &a.LeaderName, &a.WinnerID, &a.BidCount, &a.Status,
		&a.StartsAt, &a.EndsAt, &a.ClosedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func GetWith(ctx context.Context, q Querier, id string) (*Auction, error) {
	return ScanAuction(q.QueryRow(ctx, `SELECT `+SelectCols+fromJoin+` WHERE a.id=$1`, id))
}

func (s *Store) Get(ctx context.Context, id string) (*Auction, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return GetWith(ctx, s.DB, id)
}

// List filter: "" or "active" (active+scheduled), "closed" (closed+cancelled), "all".
// The WHERE/ORDER fragments are fixed strings, never user input.
func (s *Store) List(ctx context.Context, filter string) ([]Auction, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	where, order := `a.status IN ('active','scheduled')`, `a.ends_at ASC`
	switch filter {
	case "closed":
		where, order = `a.status IN ('closed','cancelled')`, `COALESCE(a.closed_at, a.ends_at) DESC`
	case "all":
		where, order = `TRUE`, `(a.status = 'active') DESC, a.ends_at DESC`
	}
	rows, err := s.DB.Query(ctx, `SELECT `+SelectCols+fromJoin+` WHERE `+where+` ORDER BY `+order+` LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Auction{}
	for rows.Next() {
		a, err := ScanAuction(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *a)
	}
	return list, rows.Err()
}

// RecentBids returns the newest non-voided bids, newest first.
func (s *Store) RecentBids(ctx context.Context, auctionID string, limit int) ([]Bid, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.DB.Query(ctx, `SELECT b.id, b.auction_id, b.user_id, p.display_name, b.amount, b.created_at
		FROM bids b JOIN profiles p ON p.user_id = b.user_id
		WHERE b.auction_id=$1 AND NOT b.voided
		ORDER BY b.created_at DESC, b.id DESC LIMIT $2`, auctionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bids := []Bid{}
	for rows.Next() {
		var b Bid
		if err := rows.Scan(&b.ID, &b.AuctionID, &b.UserID, &b.DisplayName, &b.Amount, &b.CreatedAt); err != nil {
			return nil, err
		}
		bids = append(bids, b)
	}
	return bids, rows.Err()
}
