package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"auction/internal/admin"
	"auction/internal/auction"
	"auction/internal/auth"
	"auction/internal/bid"
	"auction/internal/config"
	"auction/internal/httpx"
	"auction/internal/store"
	"auction/internal/ws"
	"auction/web"
	"auction/migrations"
)

// healthcheck is used by the Docker HEALTHCHECK: the runtime image has no shell or curl.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", "err", err)
		os.Exit(1)
	}

	// Cancelled on SIGTERM (Render deploys) or Ctrl+C locally.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.RedisURL)
	if err != nil {
		slog.Error("store error", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.Migrate(ctx, migrations.FS); err != nil {
		slog.Error("migrate error", "err", err)
		os.Exit(1)
	}
	if err := st.SeedDemoAuctions(ctx); err != nil {
		slog.Error("seed error", "err", err)
		os.Exit(1)
	}

	verifier, err := auth.NewVerifier(ctx, cfg.SupabaseURL)
	if err != nil {
		slog.Error("auth error (check SUPABASE_URL and that the project uses JWT signing keys)", "err", err)
		os.Exit(1)
	}
	am := auth.NewMiddleware(st.DB, verifier, cfg.AdminEmail)

	as := auction.NewStore(st.DB, st.Redis)
	auctions := &auction.Handler{S: as}
	bids := &bid.Handler{S: bid.NewService(as)}

	hub, err := ws.NewHub(ctx, st.Redis)
	if err != nil {
		slog.Error("realtime hub error", "err", err)
		os.Exit(1)
	}
	hub.Start()
	adm := admin.New(as, hub, cfg.DatabaseURL)

	// Background worker: starts scheduled auctions, closes expired ones.
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		as.RunWorker(ctx, 2*time.Second)
	}()

	r := chi.NewRouter()
	// Order matters: Logger wraps Recoverer so a panic is logged as a 500.
	r.Use(middleware.RequestID, httpx.Logger, httpx.Recoverer, httpx.SecurityHeaders(cfg.SupabaseURL))

	r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
		dbErr, redisErr := st.HealthCached(req.Context())
		if dbErr != nil || redisErr != nil {
			slog.Warn("unhealthy", "db", dbErr, "redis", redisErr)
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy"})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	r.Group(func(r chi.Router) {
		r.Use(am.Authenticate)

		// Public values the browser needs for Supabase login (the anon key is safe to expose).
		r.Get("/api/config", func(w http.ResponseWriter, _ *http.Request) {
			httpx.WriteJSON(w, http.StatusOK, map[string]string{
				"supabase_url": cfg.SupabaseURL, "supabase_anon_key": cfg.SupabaseAnonKey,
			})
		})

		// Guests can view auctions.
		r.Get("/api/auctions", auctions.List)
		r.Get("/api/auctions/{id}", auctions.Get)

		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Post("/api/auctions/{id}/bids", bids.Place)
			r.Get("/ws", hub.ServeWS) // live updates; token passed as ?token=
			r.Patch("/api/me", am.UpdateProfile)
			r.Get("/api/me", func(w http.ResponseWriter, req *http.Request) {
				httpx.WriteJSON(w, http.StatusOK, auth.FromContext(req.Context()))
			})
		})

		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAdmin)
			adm.Routes(r) // /api/admin/* (CSRF enforced inside)
			r.Get("/debug/stats", func(w http.ResponseWriter, req *http.Request) {
				dbErr, redisErr := st.Health(req.Context())
				httpx.WriteJSON(w, http.StatusOK, map[string]any{
					"websocket":  hub.Stats(),
					"database":   health(dbErr),
					"redis":      health(redisErr),
					"goroutines": runtime.NumGoroutine(),
				})
			})
		})
	})

	// /admin page: guests go to login, non-admins get 403. (API calls are guarded separately.)
	r.With(am.AuthenticateCookie).Get("/admin", func(w http.ResponseWriter, req *http.Request) {
		u := auth.FromContext(req.Context())
		switch {
		case u == nil:
			http.Redirect(w, req, "/#/login", http.StatusSeeOther)
		case u.Banned || !u.IsAdmin():
			http.Error(w, "403 Forbidden: admins only", http.StatusForbidden)
		default:
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFileFS(w, req, web.FS, "admin.html")
		}
	})

	// Frontend (embedded in the binary). Unknown /api paths stay JSON 404s.
	files := http.FileServerFS(web.FS)
	r.Get("/*", func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			httpx.WriteError(w, httpx.ErrNotFound)
			return
		}
		w.Header().Set("Cache-Control", "no-cache") // always revalidate so deploys show up immediately
		files.ServeHTTP(w, req)
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		slog.Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// 1) stop accepting HTTP, 2) close WebSockets and drain, 3) let the worker finish.
	if err := srv.Shutdown(sctx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
	hub.Shutdown(sctx)
	select { // let the worker finish its current tick
	case <-workerDone:
	case <-time.After(5 * time.Second):
	}
}

func health(err error) string {
	if err == nil {
		return "ok"
	}
	return "error: " + err.Error()
}
