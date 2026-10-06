package bid

// These tests run against your REAL database and Redis (DATABASE_URL, REDIS_URL).
// They create their own temporary auction and users and delete them afterwards.
//
//	set -a; source .env; set +a
//	go test -race -count=1 -v ./internal/bid/

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"auction/internal/auction"
	"auction/internal/httpx"
	"auction/internal/store"
)

func newTestEnv(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	dbURL, redisURL := os.Getenv("DATABASE_URL"), os.Getenv("REDIS_URL")
	if dbURL == "" || redisURL == "" {
		t.Skip("DATABASE_URL / REDIS_URL not set (run: set -a; source .env; set +a)")
	}
	st, err := store.Open(context.Background(), dbURL, redisURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close) // registered first, so it runs last
	placeTimeout = 60 * time.Second
	return NewService(auction.NewStore(st.DB, st.Redis)), st
}

// setupAuction creates an active auction (start 1000, increment 10) and n users.
func setupAuction(t *testing.T, st *store.Store, n int, duration string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	tag := fmt.Sprintf("test-%d", time.Now().UnixNano())

	var auctionID string
	err := st.DB.QueryRow(ctx, `INSERT INTO auctions
		(title, start_price, current_price, min_increment, status, starts_at, ends_at)
		VALUES ($1, 1000, 1000, 10, 'active', now() - interval '1 minute', now() + $2::interval)
		RETURNING id`, "race "+tag, duration).Scan(&auctionID)
	if err != nil {
		t.Fatalf("create auction: %v", err)
	}
	users := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var id string
		err := st.DB.QueryRow(ctx, `INSERT INTO profiles (user_id, email, display_name)
			VALUES (gen_random_uuid(), $1, $2) RETURNING user_id`,
			fmt.Sprintf("%s-%d@test.invalid", tag, i), fmt.Sprintf("tester%d", i)).Scan(&id)
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		users = append(users, id)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = st.DB.Exec(cctx, `DELETE FROM auctions WHERE id=$1`, auctionID) // bids cascade
		_, _ = st.DB.Exec(cctx, `DELETE FROM profiles WHERE email LIKE $1`, tag+"%")
	})
	return auctionID, users
}

// 100 different users bid at the same instant, each with a different amount.
// The highest amount can always win, so the final state is fully predictable.
func TestConcurrentBids(t *testing.T) {
	svc, st := newTestEnv(t)
	const n = 100
	auctionID, users := setupAuction(t, st, n, "1 hour")

	var accepted, tooLow atomic.Int64
	var mu sync.Mutex
	var unexpected []error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together
			_, err := svc.place(context.Background(), PlaceInput{
				AuctionID: auctionID, UserID: users[i], DisplayName: fmt.Sprintf("tester%d", i),
				Amount:         int64(1010 + 10*i),
				IdempotencyKey: fmt.Sprintf("race-%d-%d", i, time.Now().UnixNano()),
			})
			switch {
			case err == nil:
				accepted.Add(1)
			case errors.Is(err, httpx.ErrBidTooLow):
				tooLow.Add(1)
			default:
				mu.Lock()
				unexpected = append(unexpected, err)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range unexpected {
		t.Errorf("unexpected error: %v", err)
	}
	if accepted.Load()+tooLow.Load() != n {
		t.Errorf("accepted(%d)+too_low(%d) != %d", accepted.Load(), tooLow.Load(), n)
	}
	t.Logf("accepted=%d rejected(too low)=%d", accepted.Load(), tooLow.Load())

	ctx := context.Background()
	a, err := auction.GetWith(ctx, st.DB, auctionID)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(1000 + 10*n); a.CurrentPrice != want {
		t.Errorf("final price = %d, want %d", a.CurrentPrice, want)
	}
	if a.LeaderID == nil || *a.LeaderID != users[n-1] {
		t.Errorf("leader is not the highest bidder")
	}
	if int64(a.BidCount) != accepted.Load() {
		t.Errorf("bid_count = %d, accepted = %d", a.BidCount, accepted.Load())
	}

	// Every stored bid must beat the previous one by at least the increment.
	rows, err := st.DB.Query(ctx, `SELECT amount FROM bids WHERE auction_id=$1 AND NOT voided ORDER BY amount`, auctionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	prev, count := int64(1000), int64(0)
	for rows.Next() {
		var amt int64
		if err := rows.Scan(&amt); err != nil {
			t.Fatal(err)
		}
		if amt-prev < 10 {
			t.Errorf("bid %d is less than 10 above previous %d", amt, prev)
		}
		prev, count = amt, count+1
	}
	if count != accepted.Load() {
		t.Errorf("bids stored = %d, accepted = %d", count, accepted.Load())
	}
}

// One user double-taps: 20 simultaneous requests with the SAME idempotency key
// must produce exactly one bid, and every request must succeed.
func TestIdempotentDoubleTap(t *testing.T) {
	svc, st := newTestEnv(t)
	auctionID, users := setupAuction(t, st, 1, "1 hour")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.place(context.Background(), PlaceInput{
				AuctionID: auctionID, UserID: users[0], DisplayName: "tester0",
				Amount: 1010, IdempotencyKey: "double-tap-key-1",
			})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		t.Errorf("request failed: %v", err)
	}

	var bids int
	if err := st.DB.QueryRow(context.Background(), `SELECT count(*) FROM bids WHERE auction_id=$1`, auctionID).Scan(&bids); err != nil {
		t.Fatal(err)
	}
	if bids != 1 {
		t.Errorf("bids stored = %d, want exactly 1", bids)
	}
	a, _ := auction.GetWith(context.Background(), st.DB, auctionID)
	if a == nil || a.BidCount != 1 || a.CurrentPrice != 1010 {
		t.Errorf("auction state wrong: %+v", a)
	}
}

// A bid in the last 30 seconds pushes ends_at out by 30 seconds.
func TestAntiSniping(t *testing.T) {
	svc, st := newTestEnv(t)
	auctionID, users := setupAuction(t, st, 1, "10 seconds")
	ctx := context.Background()

	before, err := auction.GetWith(ctx, st.DB, auctionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.place(ctx, PlaceInput{AuctionID: auctionID, UserID: users[0], DisplayName: "tester0",
		Amount: 1010, IdempotencyKey: "snipe-key-0001"}); err != nil {
		t.Fatal(err)
	}
	after, err := auction.GetWith(ctx, st.DB, auctionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.EndsAt.Sub(before.EndsAt); got != 30*time.Second {
		t.Errorf("ends_at moved by %v, want 30s", got)
	}
}
