// Package auth verifies Supabase JWTs and enforces roles.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Claims are the parts of a Supabase access token we use.
type Claims struct {
	jwt.RegisteredClaims
	Email        string `json:"email"`
	UserMetadata struct {
		DisplayName   string `json:"display_name"`
		FullName      string `json:"full_name"`
		EmailVerified bool   `json:"email_verified"`
	} `json:"user_metadata"`
}

// Verifier checks token signatures against the project's public JWKS
// (asymmetric keys), so no shared secret is needed. Keys are cached and
// refreshed automatically in the background.
type Verifier struct {
	kf     keyfunc.Keyfunc
	parser *jwt.Parser
}

func NewVerifier(ctx context.Context, supabaseURL string) (*Verifier, error) {
	base := strings.TrimRight(supabaseURL, "/")
	kf, err := keyfunc.NewDefaultCtx(ctx, []string{base + "/auth/v1/.well-known/jwks.json"})
	if err != nil {
		return nil, fmt.Errorf("load Supabase JWKS: %w", err)
	}
	parser := jwt.NewParser(
		// Only asymmetric algorithms: blocks "alg=none" and HS256 forgery tricks.
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
		jwt.WithIssuer(base+"/auth/v1"),
		jwt.WithAudience("authenticated"),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5*time.Second),
	)
	return &Verifier{kf: kf, parser: parser}, nil
}

// Verify returns the claims of a valid, unexpired token.
func (v *Verifier) Verify(token string) (*Claims, error) {
	c := &Claims{}
	t, err := v.parser.ParseWithClaims(token, c, v.kf.Keyfunc)
	if err != nil {
		return nil, err
	}
	if !t.Valid || c.Subject == "" || c.Email == "" {
		return nil, errors.New("token missing subject or email")
	}
	return c, nil
}
