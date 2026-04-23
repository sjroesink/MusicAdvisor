package http

import (
	"database/sql"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/http/handlers"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/library"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
)

type Deps struct {
	DB             *sql.DB
	Logger         *slog.Logger
	Sessions       *auth.SessionStore
	CookieCfg      auth.CookieConfig
	Users          *user.Service
	Spotify        *spotify.Client
	LibrarySync    *library.Service
	Signals        *signal.SQLStore
	FrontendOKPath string
}

func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(chimw.Recoverer)
	r.Use(chimw.Timeout(30 * time.Second))

	r.Get("/healthz", handlers.Health(d.DB, d.Logger))

	r.Route("/api", func(api chi.Router) {
		spotifyDeps := handlers.SpotifyAuthDeps{
			DB:         d.DB,
			Logger:     d.Logger,
			Sessions:   d.Sessions,
			CookieCfg:  d.CookieCfg,
			Users:      d.Users,
			Spotify:    d.Spotify,
			FrontendOK: d.FrontendOKPath,
		}
		api.Get("/auth/spotify/login", handlers.SpotifyLogin(spotifyDeps))
		api.Get("/auth/spotify/callback", handlers.SpotifyCallback(spotifyDeps))
		api.Post("/auth/logout", handlers.Logout(d.Sessions, d.CookieCfg))

		api.Group(func(authed chi.Router) {
			authed.Use(auth.RequireAuth(d.Sessions, d.CookieCfg))
			authed.Get("/me", handlers.Me(d.DB, d.Users, d.Logger))

			syncDeps := handlers.SyncDeps{
				DB:          d.DB,
				Logger:      d.Logger,
				LibrarySync: d.LibrarySync,
			}
			authed.Post("/sync/trigger", handlers.TriggerSync(syncDeps))
			authed.Get("/sync/runs", handlers.ListSyncRuns(syncDeps))

			authed.Post("/signals", handlers.PostSignal(handlers.SignalsDeps{
				DB:      d.DB,
				Logger:  d.Logger,
				Signals: d.Signals,
			}))
		})
	})

	return r
}
