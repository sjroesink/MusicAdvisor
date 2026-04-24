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
	"github.com/sjroesink/music-advisor/backend/internal/services/lbsimilar"
	"github.com/sjroesink/music-advisor/backend/internal/services/lfsimilar"
	"github.com/sjroesink/music-advisor/backend/internal/services/library"
	"github.com/sjroesink/music-advisor/backend/internal/services/listening"
	"github.com/sjroesink/music-advisor/backend/internal/services/mbrels"
	"github.com/sjroesink/music-advisor/backend/internal/services/releases"
	"github.com/sjroesink/music-advisor/backend/internal/services/samelabel"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
	"github.com/sjroesink/music-advisor/backend/internal/services/toplists"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
	"github.com/sjroesink/music-advisor/backend/internal/sse"
)

type Deps struct {
	DB             *sql.DB
	Logger         *slog.Logger
	Sessions       *auth.SessionStore
	CookieCfg      auth.CookieConfig
	Users          *user.Service
	Spotify        *spotify.Client
	LibrarySync    *library.Service
	TopLists       *toplists.Service
	Listening      *listening.Service
	Releases       *releases.Service
	LBSimilar      *lbsimilar.Service
	MBRels         *mbrels.Service
	SameLabel      *samelabel.Service
	LFSimilar      *lfsimilar.Service
	Signals        *signal.SQLStore
	Hub            *sse.Hub
	FrontendOKPath string
	FrontendFSPath string // filesystem path to built dist; empty skips static serving
}

func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(chimw.Recoverer)

	r.Get("/healthz", handlers.Health(d.DB, d.Logger))

	r.Route("/api", func(api chi.Router) {
		// Short-lived endpoints get a 30s timeout via chi's TimeoutHandler.
		// SSE and any future websocket-style routes must NOT be under this
		// middleware — it wraps http.ResponseWriter with buffering that
		// breaks streaming.
		api.Group(func(short chi.Router) {
			short.Use(chimw.Timeout(30 * time.Second))

			spotifyDeps := handlers.SpotifyAuthDeps{
				DB:         d.DB,
				Logger:     d.Logger,
				Sessions:   d.Sessions,
				CookieCfg:  d.CookieCfg,
				Users:      d.Users,
				Spotify:    d.Spotify,
				FrontendOK: d.FrontendOKPath,
			}
			short.Get("/auth/spotify/login", handlers.SpotifyLogin(spotifyDeps))
			short.Get("/auth/spotify/callback", handlers.SpotifyCallback(spotifyDeps))
			short.Post("/auth/logout", handlers.Logout(d.Sessions, d.CookieCfg))

			short.Group(func(authed chi.Router) {
				authed.Use(auth.RequireAuth(d.Sessions, d.CookieCfg))
				authed.Get("/me", handlers.Me(d.DB, d.Users, d.Logger))

				syncDeps := handlers.SyncDeps{
					DB:          d.DB,
					Logger:      d.Logger,
					LibrarySync: d.LibrarySync,
					TopLists:    d.TopLists,
					Listening:   d.Listening,
					Releases:    d.Releases,
					LBSimilar:   d.LBSimilar,
					MBRels:      d.MBRels,
					SameLabel:   d.SameLabel,
					LFSimilar:   d.LFSimilar,
					Hub:         d.Hub,
				}
				authed.Post("/sync/trigger", handlers.TriggerSync(syncDeps))
				authed.Get("/sync/runs", handlers.ListSyncRuns(syncDeps))

				authed.Post("/signals", handlers.PostSignal(handlers.SignalsDeps{
					DB:      d.DB,
					Logger:  d.Logger,
					Signals: d.Signals,
				}))

				authed.Get("/feed", handlers.Feed(handlers.FeedDeps{
					DB:     d.DB,
					Logger: d.Logger,
				}))
			})
		})

		// Long-lived endpoints: no Timeout middleware.
		api.Group(func(stream chi.Router) {
			stream.Use(auth.RequireAuth(d.Sessions, d.CookieCfg))
			if d.Hub != nil {
				stream.Get("/feed/stream", handlers.FeedStream(handlers.FeedStreamDeps{
					Logger: d.Logger,
					Hub:    d.Hub,
				}))
			}
		})
	})

	// Static frontend (single-container deploy). Must be registered after
	// /api and /healthz so its catch-all doesn't swallow those routes.
	if fs := handlers.StaticSPA(d.FrontendFSPath, d.Logger); fs != nil {
		r.Handle("/*", fs)
	}

	return r
}
