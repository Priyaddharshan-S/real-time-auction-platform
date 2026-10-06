package bid

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"auction/internal/auth"
	"auction/internal/httpx"
)

type Handler struct{ S *Service }

type placeRequest struct {
	Amount         int64  `json:"amount"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Place: POST /api/auctions/{id}/bids   (route is wrapped in RequireAuth)
func (h *Handler) Place(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	if u == nil {
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}
	if u.IsAdmin() { // staff must not bid on their own auctions
		httpx.WriteError(w, httpx.ErrAdminCannotBid)
		return
	}
	id := chi.URLParam(r, "id")
	if !httpx.IsUUID(id) {
		httpx.WriteError(w, httpx.ErrNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req placeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.ErrBadRequest)
		return
	}
	res, err := h.S.Place(r.Context(), PlaceInput{
		AuctionID: id, UserID: u.ID, DisplayName: u.DisplayName,
		Amount: req.Amount, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
