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

	logger := newLogger(cfg.LogLevel)
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
		sigStore := sigsvc.NewSQLStore(database)
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

func newLogger(level string) *slog.Logger {
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
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
