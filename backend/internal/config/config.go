package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	BaseURL             string
	Address             string
	DatabaseURL         string // postgres DSN, e.g. postgres://user:pass@host:5432/db?sslmode=disable
	LogLevel            string
	LogFormat           string // "text" (human) or "json" (prod)
	FrontendPath        string // filesystem path to the built frontend; empty disables static serving
	SessionSecret       []byte
	SecretKey           []byte
	SpotifyClientID     string
	SpotifyClientSecret string
	LastfmAPIKey        string
	ListenBrainzToken   string
	UserAgentContact    string
	MusicBrainzBaseURL  string
	MusicBrainzRPS      float64
}

// Load reads configuration from environment variables. In dev we also pick
// up .env.local and .env from the working directory (local takes precedence,
// real OS env still wins over both). Missing files are silently ignored.
// Returns an aggregate error listing every missing or invalid field so
// operators see the full set in one pass.
func Load() (Config, error) {
	loadDotEnvFiles()

	cfg := Config{
		BaseURL:             env("MA_BASE_URL", "http://localhost:8080"),
		Address:             env("MA_ADDRESS", ":8080"),
		DatabaseURL:         env("MA_DATABASE_URL", "postgres://musicadvisor:musicadvisor@localhost:5432/musicadvisor?sslmode=disable"),
		LogLevel:            env("MA_LOG_LEVEL", "info"),
		LogFormat:           env("MA_LOG_FORMAT", "text"),
		FrontendPath:        os.Getenv("MA_FRONTEND_PATH"),
		SpotifyClientID:     os.Getenv("MA_SPOTIFY_CLIENT_ID"),
		SpotifyClientSecret: os.Getenv("MA_SPOTIFY_CLIENT_SECRET"),
		LastfmAPIKey:        os.Getenv("MA_LASTFM_API_KEY"),
		ListenBrainzToken:   os.Getenv("MA_LISTENBRAINZ_TOKEN"),
		UserAgentContact:    os.Getenv("MA_USER_AGENT_CONTACT"),
		MusicBrainzBaseURL:  env("MA_MUSICBRAINZ_BASE_URL", "http://10.0.0.170:5555/ws/2"),
		MusicBrainzRPS:      envFloat("MA_MUSICBRAINZ_RPS", 40),
	}

	var problems []string

	sessionSecret, err := readKey("MA_SESSION_SECRET", 32)
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.SessionSecret = sessionSecret

	secretKey, err := readKey("MA_SECRET_KEY", 32)
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.SecretKey = secretKey

	if len(problems) > 0 {
		return Config{}, errors.New("config: " + strings.Join(problems, "; "))
	}
	return cfg, nil
}

// loadDotEnvFiles reads .env.local then .env if present. godotenv.Load does
// NOT overwrite already-set vars, so the priority becomes:
//
//	real OS env > .env.local > .env
//
// Any file that doesn't exist is silently skipped. Real parse errors log a
// warning but don't block startup — operators can always override via env.
func loadDotEnvFiles() {
	for _, name := range []string{".env.local", ".env"} {
		info, err := os.Stat(name)
		if err != nil || info.IsDir() {
			continue
		}
		if err := godotenv.Load(name); err != nil {
			slog.Warn("config: failed to parse dotenv file", "file", name, "err", err)
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

// readKey accepts either a raw string of exact length or a hex-encoded value.
// For MVP dev we let operators paste raw strings; prod should use hex to avoid
// shell-quoting surprises.
func readKey(name string, wantLen int) ([]byte, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return nil, fmt.Errorf("%s is required (%d bytes; raw or hex-encoded)", name, wantLen)
	}
	if len(raw) == wantLen {
		return []byte(raw), nil
	}
	if len(raw) == wantLen*2 {
		// hex-encoded
		out := make([]byte, wantLen)
		for i := 0; i < wantLen; i++ {
			v, err := strconv.ParseUint(raw[i*2:i*2+2], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("%s: invalid hex", name)
			}
			out[i] = byte(v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: expected %d bytes (raw) or %d hex chars, got %d", name, wantLen, wantLen*2, len(raw))
}
