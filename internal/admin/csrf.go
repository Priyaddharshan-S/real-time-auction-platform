package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"auction/internal/auth"
	"auction/internal/httpx"
)

// CSRF design: the token is HMAC(key, userID | day) so it is per-user, stateless
// (works on every instance) and rotates daily (yesterday's is still accepted).
// The browser fetches it once, then sends it as X-CSRF-Token on every
// state-changing call. We also reject cross-origin Origin headers.

func (h *Handler) token(userID string, window int64) string {
	m := hmac.New(sha256.New, h.csrfKey)
	m.Write([]byte(userID + "|" + strconv.FormatInt(window, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

func (h *Handler) validToken(userID, got string) bool {
	now := time.Now().Unix() / 86400
	for _, win := range []int64{now, now - 1} {
		if hmac.Equal([]byte(got), []byte(h.token(userID, win))) {
			return true
		}
	}
	return false
}

func (h *Handler) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		u := auth.FromContext(r.Context())
		if u == nil || !h.validToken(u.ID, r.Header.Get("X-CSRF-Token")) {
			httpx.WriteError(w, httpx.ErrCSRF)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if pu, err := url.Parse(origin); err != nil || pu.Host != r.Host {
				httpx.WriteError(w, httpx.ErrCSRF)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// CSRFToken: GET /api/admin/csrf
func (h *Handler) CSRFToken(w http.ResponseWriter, r *http.Request) {
	u := auth.FromContext(r.Context())
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"csrf_token": h.token(u.ID, time.Now().Unix()/86400)})
}
