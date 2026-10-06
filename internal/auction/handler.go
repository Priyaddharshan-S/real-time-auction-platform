package auction

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"auction/internal/httpx"
)

// Handler serves the public (guest-readable) auction endpoints.
type Handler struct{ S *Store }

// List: GET /api/auctions?status=active|closed|all
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.S.List(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	// server_time lets the browser compute a clock offset for accurate countdowns.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auctions": list, "server_time": time.Now().UTC()})
}

// Get: GET /api/auctions/{id} returns the auction plus the latest bids.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !httpx.IsUUID(id) {
		httpx.WriteError(w, httpx.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	a, err := h.S.Get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, httpx.ErrNotFound)
		return
	}
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	bids, err := h.S.RecentBids(ctx, id, 20)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"auction": a, "bids": bids, "server_time": time.Now().UTC()})
}
