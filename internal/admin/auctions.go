package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"auction/internal/auction"
	"auction/internal/auth"
	"auction/internal/httpx"
)

const maxMoney = 1_000_000_000_000

func bad(msg string) error { return httpx.ErrBadRequest.WithMessage(msg) }

func validate(title, desc, img string, startPrice, inc int64, starts, ends time.Time) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(title)); n < 1 || n > 200 {
		return bad("Title must be 1-200 characters.")
	}
	if utf8.RuneCountInString(desc) > 5000 {
		return bad("Description is too long (max 5000 characters).")
	}
	if img != "" {
		u, err := url.Parse(img)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(img) > 500 {
			return bad("Image URL must be a valid http(s) link.")
		}
	}
	if startPrice < 0 || startPrice > maxMoney {
		return bad("Start price is out of range.")
	}
	if inc < 1 || inc > maxMoney {
		return bad("Minimum increment must be at least 1.")
	}
	// Sanity rule: a step bigger than the start price makes the first bid absurd.
	maxInc := startPrice
	if maxInc == 0 {
		maxInc = 100
	}
	if inc > maxInc {
		return bad("Minimum increment cannot be larger than the start price (max 100 if the start price is 0).")
	}
	if !ends.After(starts) {
		return bad("End time must be after the start time.")
	}
	return nil
}

// notFoundOrClosed explains a zero-row UPDATE.
func (h *Handler) notFoundOrClosed(r *http.Request, id string) error {
	ctx, cancel := h.ctx(r)
	defer cancel()
	if _, err := auction.GetWith(ctx, h.as.DB, id); errors.Is(err, pgx.ErrNoRows) {
		return httpx.ErrNotFound
	} else if err != nil {
		return err
	}
	return httpx.ErrAuctionClosed
}

// ListAuctions: GET /api/admin/auctions?status=active|closed|all (default all)
func (h *Handler) ListAuctions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	filter := r.URL.Query().Get("status")
	if filter == "" {
		filter = "all"
	}
	list, err := h.as.List(ctx, filter)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auctions": list, "server_time": time.Now().UTC()})
}

type createReq struct {
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	ImageURL     string     `json:"image_url"`
	StartPrice   int64      `json:"start_price"`
	MinIncrement int64      `json:"min_increment"`
	StartsAt     *time.Time `json:"starts_at"` // optional, default now
	EndsAt       time.Time  `json:"ends_at"`
}

// CreateAuction: POST /api/admin/auctions
func (h *Handler) CreateAuction(w http.ResponseWriter, r *http.Request) {
	admin := auth.FromContext(r.Context())
	var req createReq
	if !decode(w, r, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	starts := time.Now()
	if req.StartsAt != nil {
		starts = *req.StartsAt
	}
	if err := validate(req.Title, req.Description, req.ImageURL, req.StartPrice, req.MinIncrement, starts, req.EndsAt); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if !req.EndsAt.After(time.Now()) {
		httpx.WriteError(w, bad("End time must be in the future."))
		return
	}

	ctx, cancel := h.ctx(r)
	defer cancel()
	var id string
	err := h.as.DB.QueryRow(ctx, `INSERT INTO auctions
		(title, description, image_url, start_price, current_price, min_increment, status, starts_at, ends_at)
		VALUES ($1,$2,$3,$4,$4,$5, CASE WHEN $6::timestamptz > now() THEN 'scheduled' ELSE 'active' END, $6, $7)
		RETURNING id`,
		req.Title, req.Description, req.ImageURL, req.StartPrice, req.MinIncrement, starts, req.EndsAt).Scan(&id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	a, err := auction.GetWith(ctx, h.as.DB, id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	h.auditAfter(ctx, admin, "create_auction", id, map[string]any{"title": req.Title, "start_price": req.StartPrice, "ends_at": req.EndsAt})
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"auction": a})
}

type editReq struct {
	Title        *string    `json:"title"`
	Description  *string    `json:"description"`
	ImageURL     *string    `json:"image_url"`
	StartPrice   *int64     `json:"start_price"`
	MinIncrement *int64     `json:"min_increment"`
	StartsAt     *time.Time `json:"starts_at"`
	EndsAt       *time.Time `json:"ends_at"`
}

// EditAuction: PATCH /api/admin/auctions/{id}
// Full edit before the first bid; afterwards only title and description.
// SELECT ... FOR UPDATE locks the row, so a bid cannot slip in between the
// "has bids?" check and the update.
func (h *Handler) EditAuction(w http.ResponseWriter, r *http.Request) {
	admin := auth.FromContext(r.Context())
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req editReq
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()

	tx, err := h.as.DB.Begin(ctx)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	defer tx.Rollback(ctx)

	var cur struct {
		title, desc, img string
		startPrice, inc  int64
		bids             int
		status           string
		starts, ends     time.Time
	}
	err = tx.QueryRow(ctx, `SELECT title, description, image_url, start_price, min_increment,
		bid_count, status, starts_at, ends_at FROM auctions WHERE id=$1 FOR UPDATE`, id).
		Scan(&cur.title, &cur.desc, &cur.img, &cur.startPrice, &cur.inc, &cur.bids, &cur.status, &cur.starts, &cur.ends)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if cur.status != "active" && cur.status != "scheduled" {
		httpx.WriteError(w, httpx.ErrAuctionClosed)
		return
	}
	restricted := req.ImageURL != nil || req.StartPrice != nil || req.MinIncrement != nil || req.StartsAt != nil || req.EndsAt != nil
	if cur.bids > 0 && restricted {
		httpx.WriteError(w, httpx.ErrAuctionHasBids)
		return
	}

	changes := map[string]any{}
	note := func(field string, from, to any) { changes[field] = map[string]any{"from": from, "to": to} }
	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		if t != cur.title {
			note("title", cur.title, t)
		}
		cur.title = t
	}
	if req.Description != nil {
		if *req.Description != cur.desc {
			note("description", "(old)", "(new)")
		}
		cur.desc = *req.Description
	}
	if req.ImageURL != nil {
		cur.img = *req.ImageURL
		note("image_url", nil, cur.img)
	}
	if req.StartPrice != nil {
		note("start_price", cur.startPrice, *req.StartPrice)
		cur.startPrice = *req.StartPrice
	}
	if req.MinIncrement != nil {
		note("min_increment", cur.inc, *req.MinIncrement)
		cur.inc = *req.MinIncrement
	}
	if req.StartsAt != nil {
		note("starts_at", cur.starts, *req.StartsAt)
		cur.starts = *req.StartsAt
	}
	if req.EndsAt != nil {
		if !req.EndsAt.After(time.Now()) {
			httpx.WriteError(w, bad("End time must be in the future."))
			return
		}
		note("ends_at", cur.ends, *req.EndsAt)
		cur.ends = *req.EndsAt
	}
	if err := validate(cur.title, cur.desc, cur.img, cur.startPrice, cur.inc, cur.starts, cur.ends); err != nil {
		httpx.WriteError(w, err)
		return
	}

	// With zero bids the price follows start_price and the status follows starts_at.
	_, err = tx.Exec(ctx, `UPDATE auctions SET title=$2, description=$3, image_url=$4,
		start_price=$5,
		current_price = CASE WHEN bid_count = 0 THEN $5 ELSE current_price END,
		min_increment=$6, starts_at=$7, ends_at=$8,
		status = CASE WHEN bid_count = 0 AND $7::timestamptz > now() THEN 'scheduled' ELSE 'active' END
		WHERE id=$1`, id, cur.title, cur.desc, cur.img, cur.startPrice, cur.inc, cur.starts, cur.ends)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := audit(ctx, tx, admin, "edit_auction", id, map[string]any{"changes": changes}); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.WriteError(w, err)
		return
	}

	a, err := auction.GetWith(ctx, h.as.DB, id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	h.as.Publish(ctx, auction.Event{Type: "auction_updated", AuctionID: id, Auction: a})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auction": a})
}

// CloseAuction: POST /api/admin/auctions/{id}/close ("close now", the leader wins)
func (h *Handler) CloseAuction(w http.ResponseWriter, r *http.Request) {
	admin := auth.FromContext(r.Context())
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	a, err := h.as.Close(ctx, id, true) // atomic: WHERE status='active'; publishes auction_closed
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if a == nil {
		httpx.WriteError(w, h.notFoundOrClosed(r, id))
		return
	}
	h.auditAfter(ctx, admin, "close_auction", id, map[string]any{"winner_id": a.WinnerID, "final_price": a.CurrentPrice})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auction": a})
}

// CancelAuction: POST /api/admin/auctions/{id}/cancel (no winner)
func (h *Handler) CancelAuction(w http.ResponseWriter, r *http.Request) {
	admin := auth.FromContext(r.Context())
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	a, err := auction.ScanAuction(h.as.DB.QueryRow(ctx, `WITH u AS (
		UPDATE auctions SET status='cancelled', closed_at=now()
		WHERE id=$1 AND status IN ('active','scheduled') RETURNING *)
		SELECT `+auction.SelectCols+auction.FromCTE, id))
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, h.notFoundOrClosed(r, id))
		return
	}
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	h.as.Publish(ctx, auction.Event{Type: "auction_cancelled", AuctionID: id, Auction: a})
	h.auditAfter(ctx, admin, "cancel_auction", id, map[string]any{"bid_count": a.BidCount})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auction": a})
}

type extendReq struct {
	EndsAt  *time.Time `json:"ends_at"` // absolute new end time, or
	Minutes *int       `json:"minutes"` // add this many minutes
}

// ExtendAuction: POST /api/admin/auctions/{id}/extend  {"minutes":30} or {"ends_at":"..."}
func (h *Handler) ExtendAuction(w http.ResponseWriter, r *http.Request) {
	admin := auth.FromContext(r.Context())
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req extendReq
	if !decode(w, r, &req) {
		return
	}
	if (req.EndsAt == nil) == (req.Minutes == nil) {
		httpx.WriteError(w, bad("Send either minutes or ends_at."))
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()

	var row pgx.Row
	details := map[string]any{}
	if req.Minutes != nil {
		if *req.Minutes < 1 || *req.Minutes > 10080 {
			httpx.WriteError(w, bad("Minutes must be between 1 and 10080."))
			return
		}
		details["minutes"] = *req.Minutes
		row = h.as.DB.QueryRow(ctx, `WITH u AS (
			UPDATE auctions SET ends_at = ends_at + $2 * interval '1 minute'
			WHERE id=$1 AND status IN ('active','scheduled')
			  AND ends_at + $2 * interval '1 minute' > now() RETURNING *)
			SELECT `+auction.SelectCols+auction.FromCTE, id, *req.Minutes)
	} else {
		details["ends_at"] = *req.EndsAt
		row = h.as.DB.QueryRow(ctx, `WITH u AS (
			UPDATE auctions SET ends_at = $2::timestamptz
			WHERE id=$1 AND status IN ('active','scheduled')
			  AND $2::timestamptz > ends_at AND $2::timestamptz > now() RETURNING *)
			SELECT `+auction.SelectCols+auction.FromCTE, id, *req.EndsAt)
	}
	a, err := auction.ScanAuction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		err := h.notFoundOrClosed(r, id)
		if errors.Is(err, httpx.ErrAuctionClosed) {
			cctx, ccancel := h.ctx(r)
			defer ccancel()
			if cur, gerr := auction.GetWith(cctx, h.as.DB, id); gerr == nil && (cur.Status == "active" || cur.Status == "scheduled") {
				err = bad("The new end time must be later than the current one and in the future.")
			}
		}
		httpx.WriteError(w, err)
		return
	}
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	h.as.Publish(ctx, auction.Event{Type: "auction_updated", AuctionID: id, Auction: a})
	h.auditAfter(ctx, admin, "extend_auction", id, details)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auction": a})
}

type adminBid struct {
	ID          string    `json:"id"`
	UserID      string    `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Amount      int64     `json:"amount"`
	Voided      bool      `json:"voided"`
	CreatedAt   time.Time `json:"created_at"`
}

// AuctionBids: GET /api/admin/auctions/{id}/bids (live monitor; includes voided bids)
func (h *Handler) AuctionBids(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	a, err := auction.GetWith(ctx, h.as.DB, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	rows, err := h.as.DB.Query(ctx, `SELECT b.id, b.user_id, p.email, p.display_name, b.amount, b.voided, b.created_at
		FROM bids b JOIN profiles p ON p.user_id = b.user_id
		WHERE b.auction_id=$1 ORDER BY b.created_at DESC, b.id DESC LIMIT 200`, id)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	defer rows.Close()
	bids := []adminBid{}
	for rows.Next() {
		var b adminBid
		if err := rows.Scan(&b.ID, &b.UserID, &b.Email, &b.DisplayName, &b.Amount, &b.Voided, &b.CreatedAt); err != nil {
			httpx.WriteError(w, err)
			return
		}
		bids = append(bids, b)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auction": a, "bids": bids, "server_time": time.Now().UTC()})
}
