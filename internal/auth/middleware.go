package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"auction/internal/httpx"
)

const (
	RoleMember = "member"
	RoleAdmin  = "admin"
)

// User is loaded from the profiles table on EVERY request (not from the token),
// so bans and demotions take effect immediately.
type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Banned      bool   `json:"banned"`
}

func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

type ctxKey struct{}

// FromContext returns the signed-in user, or nil for guests.
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

type Middleware struct {
	db         *pgxpool.Pool
	verifier   *Verifier
	adminEmail string
}

func NewMiddleware(db *pgxpool.Pool, v *Verifier, adminEmail string) *Middleware {
	return &Middleware{db: db, verifier: v, adminEmail: strings.ToLower(adminEmail)}
}

// Authenticate attaches the user to the request context if a valid token is
// sent. No token = guest (allowed to continue). Bad token = 401.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			next.ServeHTTP(w, r)
			return
		}
		claims, err := m.verifier.Verify(tok)
		if err != nil {
			slog.Debug("token rejected", "err", err)
			httpx.WriteError(w, httpx.ErrUnauthorized)
			return
		}
		user, err := m.loadUser(r.Context(), claims)
		if err != nil {
			slog.Error("load profile failed", "err", err)
			httpx.WriteError(w, httpx.ErrInternal)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	})
}

// RequireAuth guards routes that need a signed-in, non-banned user.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := FromContext(r.Context())
		if u == nil {
			httpx.WriteError(w, httpx.ErrUnauthorized)
			return
		}
		if u.Banned {
			httpx.WriteError(w, httpx.ErrBanned)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdmin guards admin routes. Role comes from the DB, never the token.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := FromContext(r.Context())
		if u == nil {
			httpx.WriteError(w, httpx.ErrUnauthorized)
			return
		}
		if u.Banned {
			httpx.WriteError(w, httpx.ErrBanned)
			return
		}
		if !u.IsAdmin() {
			httpx.WriteError(w, httpx.ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken reads "Authorization: Bearer x". Browsers cannot set headers on
// WebSocket connections, so for WebSocket upgrades only we also accept ?token=.
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return r.URL.Query().Get("token")
	}
	return ""
}

const profileCols = `user_id, email, display_name, role, banned`

// loadUser fetches the profile, creating it on first login.
// ADMIN_EMAIL gets role=admin at creation, but only if the email is verified
// (otherwise anyone could sign up with that address before the owner does).
func (m *Middleware) loadUser(ctx context.Context, c *Claims) (*User, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var u User
	scan := func(row pgx.Row) error {
		return row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Banned)
	}
	err := scan(m.db.QueryRow(ctx, `SELECT `+profileCols+` FROM profiles WHERE user_id=$1`, c.Subject))
	if err == nil {
		return &u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	email := strings.ToLower(c.Email)
	role := RoleMember
	if email == m.adminEmail {
		if c.UserMetadata.EmailVerified {
			role = RoleAdmin
		} else {
			slog.Warn("ADMIN_EMAIL signed in but email is not verified; confirm your email to become admin")
		}
	}
	name := firstNonEmpty(c.UserMetadata.DisplayName, c.UserMetadata.FullName, strings.Split(email, "@")[0])
	if rs := []rune(name); len(rs) > 50 { // display names come from user input: cap the length
		name = string(rs[:50])
	}

	// ON CONFLICT handles two requests creating the same profile at once.
	err = scan(m.db.QueryRow(ctx, `INSERT INTO profiles (user_id, email, display_name, role)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
		RETURNING `+profileCols, c.Subject, email, name, role))
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// AuthenticateCookie is used ONLY for the /admin HTML page, because a browser
// page navigation cannot send an Authorization header. The cookie (Path=/admin,
// SameSite=Strict) is never read for API calls, so it creates no CSRF surface.
// A missing or bad cookie simply means "guest".
func (m *Middleware) AuthenticateCookie(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("sb_access"); err == nil && c.Value != "" {
			if claims, err := m.verifier.Verify(c.Value); err == nil {
				if user, err := m.loadUser(r.Context(), claims); err == nil {
					r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, user))
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
