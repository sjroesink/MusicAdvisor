package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	ossignal "os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/config"
	"github.com/sjroesink/music-advisor/backend/internal/db"
	mahttp "github.com/sjroesink/music-advisor/backend/internal/http"
	"github.com/sjroesink/music-advisor/backend/internal/scheduler"
	"github.com/sjroesink/music-advisor/backend/internal/providers/lastfm"
	"github.com/sjroesink/music-advisor/backend/internal/providers/listenbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/lbsimilar"
	"github.com/sjroesink/music-advisor/backend/internal/services/lfsimilar"
	"github.com/sjroesink/music-advisor/backend/internal/services/library"
	"github.com/sjroesink/music-advisor/backend/internal/services/listening"
	"github.com/sjroesink/music-advisor/backend/internal/services/mbrels"
	"github.com/sjroesink/music-advisor/backend/internal/services/releases"
	"github.com/sjroesink/music-advisor/backend/internal/services/samelabel"
	sigsvc "github.com/sjroesink/music-advisor/backend/internal/services/signal"
	"github.com/sjroesink/music-advisor/backend/internal/services/toplists"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
	"github.com/sjroesink/music-advisor/backend/internal/sse"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		os.Exit(healthcheckMain())
	}
	if len(os.Args) > 2 && os.Args[1] == "--rebuild-affinity" {
		os.Exit(rebuildAffinityMain(os.Args[2]))
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)
	logger.Info("starting music-advisor",
		"addr", cfg.Address,
		"base_url", cfg.BaseURL,
		"db_path", cfg.DatabasePath,
	)

	database, err := db.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer database.Close()

	cipher, err := auth.NewCipher(cfg.SecretKey)
	if err != nil {
		return err
	}

	sessions := auth.NewSessionStore(database)
	users := user.NewService(database, cipher)
	cookieCfg := auth.CookieFromBaseURL(cfg.BaseURL)

	spotifyClient, err := spotify.NewClient(spotify.Config{
		ClientID:     cfg.SpotifyClientID,
		ClientSecret: cfg.SpotifyClientSecret,
		RedirectURI:  strings.TrimRight(cfg.BaseURL, "/") + "/api/auth/spotify/callback",
	})
	if err != nil {
		if errors.Is(err, spotify.ErrNotConfigured) {
			logger.Warn("spotify not configured; login and sync routes return 503",
				"hint", "set MA_SPOTIFY_CLIENT_ID and MA_SPOTIFY_CLIENT_SECRET")
			spotifyClient = nil
		} else {
			return err
		}
	}

	// The signal store is always available — UI-emitted events like
	// heard_good/dismiss must work even if the Spotify-driven sync is
	// disabled.
	sigStore := sigsvc.NewSQLStore(database)

	// In-memory SSE hub. Sized for a handful of concurrent tabs per user.
	hub := sse.NewHub(32)

	// MusicBrainz + resolver + sync services — only when both Spotify and a
	// User-Agent contact are configured. MB rejects anonymous clients, so
	// skipping the sync services entirely is the honest fallback.
	var librarySync *library.Service
	var topListsSync *toplists.Service
	var listeningSvc *listening.Service
	var releasesSvc *releases.Service
	var lbSimilarSvc *lbsimilar.Service
	var mbRelsSvc *mbrels.Service
	var sameLabelSvc *samelabel.Service
	var lfSimilarSvc *lfsimilar.Service
	if spotifyClient != nil && cfg.UserAgentContact != "" {
		mbClient, err := musicbrainz.NewClient(musicbrainz.Config{
			Contact:       cfg.UserAgentContact,
			Base:          cfg.MusicBrainzBaseURL,
			RatePerSecond: cfg.MusicBrainzRPS,
		})
		if err != nil {
			return err
		}
		lbClient, err := listenbrainz.NewClient(listenbrainz.Config{
			Contact: cfg.UserAgentContact,
		})
		if err != nil {
			return err
		}
		resolverSvc := resolver.New(database, mbClient)
		librarySync = library.New(database, users, spotifyClient, resolverSvc, sigStore, logger)
		topListsSync = toplists.New(database, users, spotifyClient, resolverSvc, sigStore, logger)
		listeningSvc = listening.New(database, users, spotifyClient, resolverSvc, sigStore, logger)
		releasesSvc = releases.New(database, mbClient, logger)
		lbSimilarSvc = lbsimilar.New(database, lbClient, mbClient, logger)
		mbRelsSvc = mbrels.New(database, mbClient, logger)
		sameLabelSvc = samelabel.New(database, mbClient, logger)

		// Last.fm is optional — only build the similar-artists service if
		// an API key is configured.
		if cfg.LastfmAPIKey != "" {
			lfClient, err := lastfm.NewClient(lastfm.Config{
				APIKey:  cfg.LastfmAPIKey,
				Contact: cfg.UserAgentContact,
			})
			if err != nil {
				return err
			}
			lfSimilarSvc = lfsimilar.New(database, lfClient, mbClient, logger)
		} else {
			logger.Info("lastfm-similar disabled: MA_LASTFM_API_KEY is empty")
		}
	} else if cfg.UserAgentContact == "" {
		logger.Warn("library, toplists, listening, releases & lb-similar sync disabled: MA_USER_AGENT_CONTACT is required")
	}

	handler := mahttp.NewRouter(mahttp.Deps{
		DB:             database,
		Logger:         logger,
		Sessions:       sessions,
		CookieCfg:      cookieCfg,
		Users:          users,
		Spotify:        spotifyClient,
		LibrarySync:    librarySync,
		TopLists:       topListsSync,
		Listening:      listeningSvc,
		Releases:       releasesSvc,
		LBSimilar:      lbSimilarSvc,
		MBRels:         mbRelsSvc,
		SameLabel:      sameLabelSvc,
		LFSimilar:      lfSimilarSvc,
		Signals:        sigStore,
		Hub:            hub,
		FrontendOKPath: "/",
		FrontendFSPath: cfg.FrontendPath,
	})

	srv := &http.Server{
		Addr:              cfg.Address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	rootCtx, stop := ossignal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Build per-phase scheduler jobs from the services we actually have.
	// A nil service contributes no job; the scheduler ignores an empty
	// job slice. Services keep their own min-interval gate so duplicate
	// ticks are cheap skips.
	sched := buildScheduler(
		database, logger,
		librarySync, topListsSync, listeningSvc,
		releasesSvc, lbSimilarSvc, mbRelsSvc, sameLabelSvc, lfSimilarSvc,
	)
	schedDone := make(chan struct{})
	if sched != nil {
		go func() {
			defer close(schedDone)
			sched.Run(rootCtx)
		}()
	} else {
		close(schedDone)
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", cfg.Address)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-rootCtx.Done():
		logger.Info("shutdown requested")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	// Wait for the scheduler's current tick to drain. rootCtx already got
	// cancelled by NotifyContext's stop, so sched.Run will return on its
	// own. Give it a reasonable ceiling so a stuck sync can't block
	// shutdown forever.
	select {
	case <-schedDone:
	case <-time.After(15 * time.Second):
		logger.Warn("scheduler: shutdown wait timed out")
	}
	logger.Info("shutdown complete")
	return nil
}

// buildScheduler constructs the job list from whichever services booted
// successfully. Passing a nil service simply drops its job; the scheduler
// tolerates an empty list and returns nil itself.
func buildScheduler(
	database *sql.DB, logger *slog.Logger,
	librarySvc *library.Service,
	topListsSvc *toplists.Service,
	listeningSvc *listening.Service,
	releasesSvc *releases.Service,
	lbSimilarSvc *lbsimilar.Service,
	mbRelsSvc *mbrels.Service,
	sameLabelSvc *samelabel.Service,
	lfSimilarSvc *lfsimilar.Service,
) *scheduler.Scheduler {
	var jobs []scheduler.Job
	if librarySvc != nil {
		jobs = append(jobs, scheduler.Job{
			Name: "library", Interval: 24 * time.Hour,
			Run: func(ctx context.Context, userID string) error {
				_, err := librarySvc.Sync(ctx, userID)
				return err
			},
		})
	}
	if topListsSvc != nil {
		jobs = append(jobs, scheduler.Job{
			Name: "toplists", Interval: 24 * time.Hour,
			Run: func(ctx context.Context, userID string) error {
				_, err := topListsSvc.Sync(ctx, userID)
				return err
			},
		})
	}
	if listeningSvc != nil {
		jobs = append(jobs, scheduler.Job{
			Name: "listening", Interval: 20 * time.Minute,
			Run: func(ctx context.Context, userID string) error {
				_, err := listeningSvc.Sync(ctx, userID)
				return err
			},
		})
	}
	if releasesSvc != nil {
		jobs = append(jobs, scheduler.Job{
			Name: "mb-new-releases", Interval: 6 * time.Hour,
			Run: func(ctx context.Context, userID string) error {
				_, err := releasesSvc.Sync(ctx, userID)
				return err
			},
		})
	}
	if lbSimilarSvc != nil {
		jobs = append(jobs, scheduler.Job{
			Name: "lb-similar", Interval: 30 * time.Minute,
			Run: func(ctx context.Context, userID string) error {
				_, err := lbSimilarSvc.Sync(ctx, userID)
				return err
			},
		})
	}
	if mbRelsSvc != nil {
		jobs = append(jobs, scheduler.Job{
			Name: "mb-artist-rels", Interval: 6 * time.Hour,
			Run: func(ctx context.Context, userID string) error {
				_, err := mbRelsSvc.Sync(ctx, userID)
				return err
			},
		})
	}
	if sameLabelSvc != nil {
		jobs = append(jobs, scheduler.Job{
			Name: "mb-same-label", Interval: 6 * time.Hour,
			Run: func(ctx context.Context, userID string) error {
				_, err := sameLabelSvc.Sync(ctx, userID)
				return err
			},
		})
	}
	if lfSimilarSvc != nil {
		jobs = append(jobs, scheduler.Job{
			Name: "lastfm-similar", Interval: 1 * time.Hour,
			Run: func(ctx context.Context, userID string) error {
				_, err := lfSimilarSvc.Sync(ctx, userID)
				return err
			},
		})
	}
	if len(jobs) == 0 {
		return nil
	}
	return scheduler.New(database, logger, jobs...)
}

// rebuildAffinityMain is a one-shot migration: pre-Phase-4 syncs wrote raw
// signals without updating affinity tables. Run once per user after upgrade:
//
//	./server --rebuild-affinity <user-id>
func rebuildAffinityMain(userID string) int {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("rebuild: config", "err", err)
		return 1
	}
	database, err := db.Open(cfg.DatabasePath)
	if err != nil {
		slog.Error("rebuild: open db", "err", err)
		return 1
	}
	defer database.Close()

	store := sigsvc.NewSQLStore(database)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := store.Rebuild(ctx, userID); err != nil {
		slog.Error("rebuild: failed", "user_id", userID, "err", err)
		return 1
	}
	slog.Info("affinity rebuilt from signals", "user_id", userID)
	return 0
}

func healthcheckMain() int {
	addr := os.Getenv("MA_ADDRESS")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 1
	}
	return 0
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if strings.ToLower(format) == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
