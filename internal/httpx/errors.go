// Package httpx holds typed API errors, JSON helpers and shared HTTP middleware.
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
)

// Error is a typed API error. The Code is stable and shown to the frontend,
// which maps it to a toast message. Extra carries optional details
// (for example the minimum next bid).
type Error struct {
	Code    string
	Status  int
	Message string
	Extra   map[string]any
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Is lets errors.Is(err, ErrBidTooLow) work even on copies made by With.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// With returns a copy of the error with one extra detail attached.
func (e *Error) With(key string, val any) *Error {
	c := *e
	c.Extra = make(map[string]any, len(e.Extra)+1)
	for k, v := range e.Extra {
		c.Extra[k] = v
	}
	c.Extra[key] = val
	return &c
}

// WithMessage returns a copy with a more specific human-readable message.
func (e *Error) WithMessage(msg string) *Error {
	c := *e
	c.Message = msg
	return &c
}

func newErr(code string, status int, msg string) *Error {
	return &Error{Code: code, Status: status, Message: msg}
}

var (
	ErrUnauthorized  = newErr("unauthorized", http.StatusUnauthorized, "Please sign in.")
	ErrForbidden     = newErr("forbidden", http.StatusForbidden, "You do not have permission to do that.")
	ErrBanned        = newErr("banned", http.StatusForbidden, "Your account has been banned.")
	ErrNotFound      = newErr("not_found", http.StatusNotFound, "Not found.")
	ErrBadRequest    = newErr("bad_request", http.StatusBadRequest, "Invalid request.")
	ErrBidTooLow     = newErr("bid_too_low", http.StatusConflict, "Your bid is too low.")
	ErrAuctionClosed = newErr("auction_closed", http.StatusConflict, "This auction is not accepting bids.")
	ErrRateLimited   = newErr("rate_limited", http.StatusTooManyRequests, "Too many bids. Slow down.")
	ErrAuctionHasBids = newErr("auction_has_bids", http.StatusConflict, "Only the title and description can be edited after the first bid.")
	ErrAdminCannotBid = newErr("admin_cannot_bid", http.StatusForbidden, "Admins cannot place bids. Use a member account.")
	ErrCSRF           = newErr("csrf_invalid", http.StatusForbidden, "Missing or invalid CSRF token. Reload the page.")
	ErrInternal      = newErr("internal", http.StatusInternalServerError, "Something went wrong.")
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s looks like a UUID (checked before it reaches Postgres).
func IsUUID(s string) bool { return uuidRe.MatchString(s) }

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError maps any error to {"error":{"code","message","details"?}}.
// Unknown errors become a generic 500; the real error is logged, never leaked.
func WriteError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		slog.Error("unhandled error", "err", err)
		e = ErrInternal
	}
	body := map[string]any{"code": e.Code, "message": e.Message}
	if len(e.Extra) > 0 {
		body["details"] = e.Extra
	}
	WriteJSON(w, e.Status, map[string]any{"error": body})
}
