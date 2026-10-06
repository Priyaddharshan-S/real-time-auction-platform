package httpx

import (
	"net/http"
	"strings"
)

// SecurityHeaders adds a strict Content-Security-Policy and other standard
// browser protections. Scripts may only load from our own origin (no inline
// scripts), which blocks most XSS even if some text were ever left unescaped.
func SecurityHeaders(supabaseURL string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			csp := strings.Join([]string{
				"default-src 'self'",
				"script-src 'self'",
				"style-src 'self' 'unsafe-inline'",
				"img-src 'self' https: http: data:",
				"connect-src 'self' " + supabaseURL + " ws://" + r.Host + " wss://" + r.Host,
				"frame-ancestors 'none'", "base-uri 'self'", "form-action 'self'", "object-src 'none'",
			}, "; ")
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			if r.Header.Get("X-Forwarded-Proto") == "https" { // Render terminates TLS in front of us
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
