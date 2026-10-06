package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// SeedDemoAuctions inserts 3 demo auctions if the auctions table is empty.
// The advisory lock makes concurrent instances seed only once.
func (s *Store) SeedDemoAuctions(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrateLockKey+1); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM auctions`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	demos := []struct {
		title, desc, img string
		price, inc       int64
		dur              time.Duration
	}{
		{"Vintage Film Camera", "Fully working 35mm camera from the 1970s.", "https://picsum.photos/seed/camera/600/400", 50, 5, 2 * time.Hour},
		{"Mechanical Keyboard", "Hand-built 75% keyboard with custom keycaps.", "https://picsum.photos/seed/keyboard/600/400", 100, 10, 6 * time.Hour},
		{"Signed First Edition Book", "Rare signed first edition in excellent condition.", "https://picsum.photos/seed/book/600/400", 200, 20, 24 * time.Hour},
	}
	for _, d := range demos {
		_, err := tx.Exec(ctx, `INSERT INTO auctions
			(title, description, image_url, start_price, current_price, min_increment, status, starts_at, ends_at)
			VALUES ($1,$2,$3,$4,$4,$5,'active', now(), now() + $6::interval)`,
			d.title, d.desc, d.img, d.price, d.inc, fmt.Sprintf("%d seconds", int(d.dur.Seconds())))
		if err != nil {
			return err
		}
	}
	slog.Info("seeded demo auctions", "count", len(demos))
	return tx.Commit(ctx)
}
