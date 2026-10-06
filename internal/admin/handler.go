// Package admin holds the /api/admin/* endpoints. Every route here sits behind
// RequireAdmin (role loaded from the DB per request) AND the CSRF check below.
package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"auction/internal/auction"
	"auction/internal/auth"
	"auction/internal/httpx"
	"auction/internal/ws"
)

type Handler struct {
	as      *auction.Store
	hub     *ws.Hub
	csrfKey []byte
}

// secret is any value that is private and identical on every instance
// (we pass DATABASE_URL); it only seeds the CSRF token HMAC key.
func New(as *auction.Store, hub *ws.Hub, secret string) *Handler {
	k := sha256.Sum256([]byte("auction-csrf|" + secret))
	return &Handler{as: as, hub: hub, csrfKey: k[:]}
}

// Routes registers everything under /api/admin. Call it inside a RequireAdmin group.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/api/admin", func(r chi.Router) {
		r.Use(h.csrf) // enforced on POST/PATCH/PUT/DELETE only

		r.Get("/csrf", h.CSRFToken)
		r.Get("/dashboard", h.Dashboard)

		r.Get("/auctions", h.ListAuctions)
		r.Post("/auctions", h.CreateAuction)
		r.Patch("/auctions/{id}", h.EditAuction)
		r.Post("/auctions/{id}/close", h.CloseAuction)
		r.Post("/auctions/{id}/cancel", h.CancelAuction)
		r.Post("/auctions/{id}/extend", h.ExtendAuction)
		r.Get("/auctions/{id}/bids", h.AuctionBids)

		r.Get("/users", h.ListUsers)
		r.Post("/users/{id}/ban", h.setBanned(true))
		r.Post("/users/{id}/unban", h.setBanned(false))
		r.Post("/users/{id}/promote", h.setRole(true))
		r.Post("/users/{id}/demote", h.setRole(false))

		r.Post("/bids/{id}/void", h.VoidBid)

		r.Get("/clients", h.ListClients)
		r.Post("/clients/{id}/kick", h.KickClient)

		r.Get("/results", h.Results)
		r.Get("/results.csv", h.ResultsCSV)
		r.Get("/audit", h.AuditLog)
	})
}

// ---- small helpers shared by the handler files ----

func (h *Handler) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 8*time.Second)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		httpx.WriteError(w, httpx.ErrBadRequest.WithMessage("Invalid JSON body."))
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := chi.URLParam(r, "id")
	if !httpx.IsUUID(id) {
		httpx.WriteError(w, httpx.ErrNotFound)
		return "", false
	}
	return id, true
}

type rowScanner interface{ Scan(dest ...any) error }

// ---- audit log ----

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// audit records an admin action. Pass the transaction when the action itself
// runs in one, so the change and its audit row commit (or roll back) together.
// auctionID is nil or a UUID string.
func audit(ctx context.Context, q execer, admin *auth.User, action string, auctionID any, details map[string]any) error {
	b, _ := json.Marshal(details)
	_, err := q.Exec(ctx, `INSERT INTO admin_actions (admin_id, action, auction_id, details)
		VALUES ($1, $2, $3, $4::jsonb)`, admin.ID, action, auctionID, string(b))
	return err
}

func (h *Handler) auditAfter(ctx context.Context, admin *auth.User, action string, auctionID any, details map[string]any) {
	if err := audit(ctx, h.as.DB, admin, action, auctionID, details); err != nil {
		slog.Error("audit write failed", "action", action, "err", err)
	}
}

func chiParam(r *http.Request, name string) string { return chi.URLParam(r, name) }
