package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"auction/internal/httpx"
)

// UpdateProfile: PATCH /api/me  {"display_name":"..."}
// Lets a member choose the name other bidders see. Past bids show the new name
// too, because bid history joins to the profile.
func (m *Middleware) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	u := FromContext(r.Context())
	if u == nil {
		httpx.WriteError(w, httpx.ErrUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.ErrBadRequest)
		return
	}
	name := strings.Join(strings.Fields(req.DisplayName), " ") // trim and collapse spaces
	if n := utf8.RuneCountInString(name); n < 2 || n > 30 {
		httpx.WriteError(w, httpx.ErrBadRequest.WithMessage("Display name must be 2-30 characters."))
		return
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			httpx.WriteError(w, httpx.ErrBadRequest.WithMessage("Display name contains invalid characters."))
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var out User
	err := m.db.QueryRow(ctx, `UPDATE profiles SET display_name=$2 WHERE user_id=$1 RETURNING `+profileCols, u.ID, name).
		Scan(&out.ID, &out.Email, &out.DisplayName, &out.Role, &out.Banned)
	if err != nil {
		httpx.WriteError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
