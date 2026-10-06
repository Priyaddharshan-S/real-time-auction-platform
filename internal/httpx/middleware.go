package httpx

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// Logger writes one structured log line per request, including the request ID.
// It logs r.URL.Path only (never the query string, which may hold a WS token).
func Logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// chi's wrapper keeps http.Hijacker support, which WebSockets need.
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		slog.Info("request",
			"request_id", middleware.GetReqID(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// Recoverer turns a panic into a logged 500 JSON error instead of a dropped connection.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.Error("panic recovered",
					"request_id", middleware.GetReqID(r.Context()),
					"panic", rec,
					"stack", string(debug.Stack()),
				)
				WriteError(w, ErrInternal)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
