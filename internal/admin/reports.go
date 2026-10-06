package admin

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"auction/internal/httpx"
)

// Dashboard: GET /api/admin/dashboard
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := h.ctx(r)
	defer cancel()

	var active, scheduled, users, banned, perMinute int
	err := h.as.DB.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status='active'), count(*) FILTER (WHERE status='scheduled') FROM auctions`).
		Scan(&active, &scheduled)
	if err == nil {
		err = h.as.DB.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE banned) FROM profiles`).Scan(&users, &banned)
	}
	if err == nil {
		err = h.as.DB.QueryRow(ctx, `SELECT count(*) FROM bids WHERE created_at > now() - interval '1 minute'`).Scan(&perMinute)
	}
	if err != nil {
		httpx.WriteError(w, err)
		return
	}

	type point struct {
		Minute time.Time `json:"minute"`
		Bids   int       `json:"bids"`
	}
	series := []point{}
	rows, err := h.as.DB.Query(ctx, `SELECT date_trunc('minute', created_at) AS m, count(*)
		FROM bids WHERE created_at > now() - interval '15 minutes' GROUP BY 1 ORDER BY 1`)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var p point
		if err := rows.Scan(&p.Minute, &p.Bids); err != nil {
			httpx.WriteError(w, err)
			return
		}
		series = append(series, p)
	}

	status := func(err error) string {
		if err == nil {
			return "ok"
		}
		return "error"
	}
	hctx, hcancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer hcancel()
	dbErr := h.as.DB.Ping(hctx)
	redisErr := h.as.Redis.Ping(hctx).Err()

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"active_auctions":     active,
		"scheduled_auctions":  scheduled,
		"users":               users,
		"banned_users":        banned,
		"bids_last_minute":    perMinute,
		"bids_per_minute_15m": series,
		"connected_clients":   h.hub.Stats().Clients, // this instance
		"database":            status(dbErr),
		"redis":               status(redisErr),
	})
}

type resultRow struct {
	AuctionID   string     `json:"auction_id"`
	Title       string     `json:"title"`
	FinalPrice  int64      `json:"final_price"`
	BidCount    int        `json:"bid_count"`
	ClosedAt    *time.Time `json:"closed_at"`
	WinnerID    *string    `json:"winner_id"`
	WinnerName  *string    `json:"winner_name"`
	WinnerEmail *string    `json:"winner_email"`
}

func (h *Handler) loadResults(ctx context.Context) ([]resultRow, error) {
	rows, err := h.as.DB.Query(ctx, `SELECT a.id, a.title, a.current_price, a.bid_count, a.closed_at,
		a.winner_id, w.display_name, w.email
		FROM auctions a LEFT JOIN profiles w ON w.user_id = a.winner_id
		WHERE a.status='closed' ORDER BY a.closed_at DESC NULLS LAST LIMIT 1000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []resultRow{}
	for rows.Next() {
		var x resultRow
		if err := rows.Scan(&x.AuctionID, &x.Title, &x.FinalPrice, &x.BidCount, &x.ClosedAt,
			&x.WinnerID, &x.WinnerName, &x.WinnerEmail); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Results: GET /api/admin/results
func (h *Handler) Results(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.loadResults(ctx)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"results": res})
}

// csvSafe blocks spreadsheet formula injection: titles and names are user input,
// and a cell starting with = + - @ would run as a formula in Excel.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ResultsCSV: GET /api/admin/results.csv (the browser fetches it with the token, then saves it)
func (h *Handler) ResultsCSV(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	res, err := h.loadResults(ctx)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="auction-results.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"auction_id", "title", "final_price", "bid_count", "closed_at", "winner_name", "winner_email"})
	for _, x := range res {
		closed := ""
		if x.ClosedAt != nil {
			closed = x.ClosedAt.UTC().Format(time.RFC3339)
		}
		_ = cw.Write([]string{x.AuctionID, csvSafe(x.Title), strconv.FormatInt(x.FinalPrice, 10),
			strconv.Itoa(x.BidCount), closed, csvSafe(deref(x.WinnerName)), csvSafe(deref(x.WinnerEmail))})
	}
	cw.Flush()
}

type auditRow struct {
	ID         int64           `json:"id"`
	AdminID    string          `json:"admin_id"`
	AdminEmail *string         `json:"admin_email"`
	Action     string          `json:"action"`
	AuctionID  *string         `json:"auction_id"`
	AuctionTitle *string       `json:"auction_title"`
	Details    json.RawMessage `json:"details"`
	CreatedAt  time.Time       `json:"created_at"`
}

// AuditLog: GET /api/admin/audit?limit=100&auction_id=...
func (h *Handler) AuditLog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := h.ctx(r)
	defer cancel()
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	var auctionFilter any // nil = all auctions
	if id := r.URL.Query().Get("auction_id"); id != "" {
		if !httpx.IsUUID(id) {
			httpx.WriteError(w, bad("Invalid auction_id."))
			return
		}
		auctionFilter = id
	}
	rows, err := h.as.DB.Query(ctx, `SELECT x.id, x.admin_id, p.email, x.action, x.auction_id, au.title, x.details, x.created_at
		FROM admin_actions x LEFT JOIN profiles p ON p.user_id = x.admin_id
		LEFT JOIN auctions au ON au.id = x.auction_id
		WHERE ($1::uuid IS NULL OR x.auction_id = $1::uuid)
		ORDER BY x.created_at DESC, x.id DESC LIMIT $2`, auctionFilter, limit)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	defer rows.Close()
	out := []auditRow{}
	for rows.Next() {
		var a auditRow
		var details []byte
		if err := rows.Scan(&a.ID, &a.AdminID, &a.AdminEmail, &a.Action, &a.AuctionID, &a.AuctionTitle, &details, &a.CreatedAt); err != nil {
			httpx.WriteError(w, err)
			return
		}
		a.Details = json.RawMessage(details)
		out = append(out, a)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"actions": out})
}
