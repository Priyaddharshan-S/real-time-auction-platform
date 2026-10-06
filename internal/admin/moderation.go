package admin

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"auction/internal/auction"
	"auction/internal/auth"
	"auction/internal/httpx"
)

const userCols = `user_id, email, display_name, role, banned, created_at`

type userView struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Banned      bool      `json:"banned"`
	CreatedAt   time.Time `json:"created_at"`
}

func scanUser(row rowScanner) (*userView, error) {
	var u userView
	if err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Banned, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers: GET /api/admin/users?q=text (matches email or display name)
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	q := r.URL.Query().Get("q")
	rows, err := h.as.DB.Query(ctx, `SELECT `+userCols+` FROM profiles
		WHERE $1 = '' OR position(lower($1) in lower(email)) > 0 OR position(lower($1) in lower(display_name)) > 0
		ORDER BY created_at DESC LIMIT 100`, q)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	defer rows.Close()
	users := []userView{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		users = append(users, *u)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": users})
}

// setBanned builds the ban / unban handlers. Banning also kicks the user's
// WebSockets on every instance. Admins cannot be banned (demote first).
func (h *Handler) setBanned(banned bool) http.HandlerFunc {
	action := "unban_user"
	if banned {
		action = "ban_user"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		admin := auth.FromContext(r.Context())
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if banned && id == admin.ID {
			httpx.WriteError(w, bad("You cannot ban yourself."))
			return
		}
		ctx, cancel := h.ctx(r)
		defer cancel()

		u, err := scanUser(h.as.DB.QueryRow(ctx, `UPDATE profiles SET banned=$2
			WHERE user_id=$1 AND (NOT $2::boolean OR role <> 'admin') RETURNING `+userCols, id, banned))
		if errors.Is(err, pgx.ErrNoRows) {
			var role string
			serr := h.as.DB.QueryRow(ctx, `SELECT role FROM profiles WHERE user_id=$1`, id).Scan(&role)
			if errors.Is(serr, pgx.ErrNoRows) {
				httpx.WriteError(w, httpx.ErrNotFound)
			} else {
				httpx.WriteError(w, httpx.ErrForbidden.WithMessage("Demote this admin before banning."))
			}
			return
		}
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		kicked := 0
		if banned {
			kicked = h.hub.KickUser(ctx, id)
		}
		h.auditAfter(ctx, admin, action, nil, map[string]any{"user_id": id, "email": u.Email})
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": u, "sockets_kicked_here": kicked})
	}
}

// setRole builds the promote / demote handlers. An advisory lock serialises role
// changes so two admins cannot demote each other and leave zero admins.
func (h *Handler) setRole(promote bool) http.HandlerFunc {
	action := "demote_user"
	if promote {
		action = "promote_user"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		admin := auth.FromContext(r.Context())
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if !promote && id == admin.ID {
			httpx.WriteError(w, bad("You cannot demote yourself."))
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
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(727300)`); err != nil {
			httpx.WriteError(w, err)
			return
		}

		sql := `UPDATE profiles SET role='member' WHERE user_id=$1 AND role='admin'
			AND (SELECT count(*) FROM profiles WHERE role='admin' AND NOT banned) > 1 RETURNING ` + userCols
		if promote {
			sql = `UPDATE profiles SET role='admin' WHERE user_id=$1 AND NOT banned AND role <> 'admin' RETURNING ` + userCols
		}
		u, err := scanUser(tx.QueryRow(ctx, sql, id))
		if errors.Is(err, pgx.ErrNoRows) {
			var role string
			var banned bool
			serr := tx.QueryRow(ctx, `SELECT role, banned FROM profiles WHERE user_id=$1`, id).Scan(&role, &banned)
			switch {
			case errors.Is(serr, pgx.ErrNoRows):
				httpx.WriteError(w, httpx.ErrNotFound)
			case promote && role == "admin":
				httpx.WriteError(w, bad("This user is already an admin."))
			case promote && banned:
				httpx.WriteError(w, bad("Unban this user before promoting."))
			case !promote && role != "admin":
				httpx.WriteError(w, bad("This user is not an admin."))
			default:
				httpx.WriteError(w, httpx.ErrForbidden.WithMessage("Cannot demote the last admin."))
			}
			return
		}
		if err != nil {
			httpx.WriteError(w, err)
			return
		}
		if err := audit(ctx, tx, admin, action, nil, map[string]any{"user_id": id, "email": u.Email}); err != nil {
			httpx.WriteError(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			httpx.WriteError(w, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": u})
	}
}

// recomputeSQL rebuilds price, leader and bid count from the NON-voided bids.
// If the auction is already closed the winner is corrected too.
const recomputeSQL = `WITH top AS (
	SELECT user_id, amount FROM bids
	WHERE auction_id=$1 AND NOT voided
	ORDER BY amount DESC, created_at ASC, id ASC LIMIT 1
), u AS (
	UPDATE auctions SET
		current_price = COALESCE((SELECT amount FROM top), start_price),
		leader_id     = (SELECT user_id FROM top),
		winner_id     = CASE WHEN status = 'closed' THEN (SELECT user_id FROM top) ELSE winner_id END,
		bid_count     = (SELECT count(*) FROM bids WHERE auction_id=$1 AND NOT voided)
	WHERE id=$1 RETURNING *)
SELECT ` + auction.SelectCols + auction.FromCTE

// VoidBid: POST /api/admin/bids/{id}/void
// One transaction. Locking order is auction row first (same as placing a bid),
// so voiding and bidding can never deadlock or interleave.
func (h *Handler) VoidBid(w http.ResponseWriter, r *http.Request) {
	admin := auth.FromContext(r.Context())
	id, ok := pathID(w, r)
	if !ok {
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

	var auctionID string
	err = tx.QueryRow(ctx, `SELECT auction_id FROM bids WHERE id=$1`, id).Scan(&auctionID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM auctions WHERE id=$1 FOR UPDATE`, auctionID).Scan(&status); err != nil {
		httpx.WriteError(w, err)
		return
	}

	var amount int64
	var bidder string
	err = tx.QueryRow(ctx, `UPDATE bids SET voided=true WHERE id=$1 AND NOT voided RETURNING amount, user_id`, id).
		Scan(&amount, &bidder)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, bad("This bid is already voided."))
		return
	}
	if err != nil {
		httpx.WriteError(w, err)
		return
	}

	a, err := auction.ScanAuction(tx.QueryRow(ctx, recomputeSQL, auctionID))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := audit(ctx, tx, admin, "void_bid", auctionID, map[string]any{
		"bid_id": id, "amount": amount, "bidder_id": bidder, "new_price": a.CurrentPrice}); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		httpx.WriteError(w, err)
		return
	}

	h.as.Publish(ctx, auction.Event{Type: "bid_voided", AuctionID: auctionID, Auction: a,
		Bid: &auction.Bid{ID: id, AuctionID: auctionID, UserID: bidder, Amount: amount}})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auction": a})
}

// ListClients: GET /api/admin/clients (sockets on THIS instance)
func (h *Handler) ListClients(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"clients": h.hub.Snapshot(),
		"note":    "sockets connected to this server instance",
	})
}

var clientIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// KickClient: POST /api/admin/clients/{id}/kick
func (h *Handler) KickClient(w http.ResponseWriter, r *http.Request) {
	admin := auth.FromContext(r.Context())
	cid := chiParam(r, "id")
	if !clientIDRe.MatchString(cid) {
		httpx.WriteError(w, httpx.ErrNotFound)
		return
	}
	ctx, cancel := h.ctx(r)
	defer cancel()
	n := h.hub.KickClient(ctx, cid)
	h.auditAfter(ctx, admin, "kick_client", nil, map[string]any{"client_id": cid})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"kicked_here": n, "requested_on_all_instances": true})
}
