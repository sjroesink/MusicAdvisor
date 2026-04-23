// Package spotify is a narrow client for the bits of the Spotify Web API
// that Music Advisor needs. Phase 2 covers OAuth and the /me endpoint;
// later phases extend it with library, top-lists, recently-played, and
// playlists without changing the shape of this type.
package spotify

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultAuthBase  = "https://accounts.spotify.com"
	DefaultAPIBase   = "https://api.spotify.com/v1"
	defaultUserAgent = "MusicAdvisor/0.1"
)

// Scopes we ask for on every login. Listed in spec section 5.1.
var DefaultScopes = []string{
	"user-library-read",
	"user-follow-read",
	"user-top-read",
	"user-read-recently-played",
	"playlist-read-private",
	"playlist-read-collaborative",
	"user-read-email",
}

// Client is the low-level HTTP client. Callers build requests via the
// higher-level methods on this package (OAuth, Me); Client.Do handles
// auth headers and basic error shaping.
type Client struct {
	httpClient   *http.Client
	authBase     string
	apiBase      string
	clientID     string
	clientSecret string
	redirectURI  string
	userAgent    string
}

// Config is what the server wires into NewClient.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string       // full URL, e.g. http://localhost:8080/api/auth/spotify/callback
	UserAgent    string       // optional; defaults to "MusicAdvisor/0.1"
	HTTPClient   *http.Client // optional; defaults to &http.Client{Timeout: 15s}
	AuthBase     string       // optional; overridden in tests
	APIBase      string       // optional; overridden in tests
}

// NewClient validates config and returns a ready client. Missing ClientID
// or ClientSecret returns nil + error so callers can surface a friendly
// "Spotify not configured" 503 instead of crashing at startup.
func NewClient(cfg Config) (*Client, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, ErrNotConfigured
	}
	if cfg.RedirectURI == "" {
		return nil, ErrNoRedirectURI
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.AuthBase == "" {
		cfg.AuthBase = DefaultAuthBase
	}
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultAPIBase
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	return &Client{
		httpClient:   cfg.HTTPClient,
		authBase:     cfg.AuthBase,
		apiBase:      cfg.APIBase,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		redirectURI:  cfg.RedirectURI,
		userAgent:    ua,
	}, nil
}

func (c *Client) RedirectURI() string { return c.redirectURI }

// Name implements the Provider interface shape used in later phases.
func (c *Client) Name() string { return "spotify" }

// NewPKCE returns a (verifier, challenge) pair for a fresh OAuth flow.
// Verifier: 32 random bytes base64url-encoded → 43 chars, within RFC 7636.
// Challenge: SHA256(verifier) base64url-encoded.
func NewPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// NewState returns a random opaque state token for CSRF protection on OAuth.
func NewState() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// AuthorizeURL builds the URL the browser should visit to start OAuth.
func (c *Client) AuthorizeURL(state, codeChallenge string, scopes []string) string {
	if scopes == nil {
		scopes = DefaultScopes
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.clientID},
		"redirect_uri":          {c.redirectURI},
		"state":                 {state},
		"scope":                 {strings.Join(scopes, " ")},
		"code_challenge_method": {"S256"},
		"code_challenge":        {codeChallenge},
	}
	return c.authBase + "/authorize?" + q.Encode()
}
