package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	BaseURL            string
	Address            string
	DatabasePath       string
	LogLevel           string
	SessionSecret      []byte
	SecretKey          []byte
	SpotifyClientID    string
	SpotifyClientSecret string
	LastfmAPIKey       string
	ListenBrainzToken  string
	UserAgentContact   string
}

// Load reads configuration from environment variables. Returns an aggregate
// error listing every missing or invalid field so operators see the full set
// in one pass.
func Load() (Config, error) {
	cfg := Config{
		BaseURL:             env("MA_BASE_URL", "http://localhost:8080"),
		Address:             env("MA_ADDRESS", ":8080"),
		DatabasePath:        env("MA_DATABASE_PATH", "./data/music-advisor.db"),
		LogLevel:            env("MA_LOG_LEVEL", "info"),
		SpotifyClientID:     os.Getenv("MA_SPOTIFY_CLIENT_ID"),
		SpotifyClientSecret: os.Getenv("MA_SPOTIFY_CLIENT_SECRET"),
		LastfmAPIKey:        os.Getenv("MA_LASTFM_API_KEY"),
		ListenBrainzToken:   os.Getenv("MA_LISTENBRAINZ_TOKEN"),
		UserAgentContact:    os.Getenv("MA_USER_AGENT_CONTACT"),
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

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
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
