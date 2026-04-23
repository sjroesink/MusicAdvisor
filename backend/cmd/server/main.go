package main

import (
	"context"
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
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/library"
	sigsvc "github.com/sjroesink/music-advisor/backend/internal/services/signal"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
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

	// MusicBrainz + resolver + library sync — only when both Spotify and a
	// User-Agent contact are configured. MB rejects anonymous clients, so
	// skipping the sync service entirely is the honest fallback.
	var librarySync *library.Service
	if spotifyClient != nil && cfg.UserAgentContact != "" {
		mbClient, err := musicbrainz.NewClient(musicbrainz.Config{
			Contact: cfg.UserAgentContact,
		})
		if err != nil {
			return err
		}
		resolverSvc := resolver.New(database, mbClient)
		librarySync = library.New(database, users, spotifyClient, resolverSvc, sigStore, logger)
	} else if cfg.UserAgentContact == "" {
		logger.Warn("library sync disabled: MA_USER_AGENT_CONTACT is required by MusicBrainz")
	}

	handler := mahttp.NewRouter(mahttp.Deps{
		DB:             database,
		Logger:         logger,
		Sessions:       sessions,
		CookieCfg:      cookieCfg,
		Users:          users,
		Spotify:        spotifyClient,
		LibrarySync:    librarySync,
		Signals:        sigStore,
		FrontendOKPath: "/",
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
	logger.Info("shutdown complete")
	return nil
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
