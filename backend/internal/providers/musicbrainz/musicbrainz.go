// Package musicbrainz is a narrow client for the MusicBrainz Web Service v2.
// All outbound calls pass through a shared 1 req/s global token bucket;
// MusicBrainz bans aggressive clients, so concurrent callers MUST go through
// this package rather than the net/http default.
package musicbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/time/rate"
)

const (
	DefaultBase     = "https://musicbrainz.org/ws/2"
	defaultAppName  = "MusicAdvisor"
	defaultVersion  = "0.1"
	defaultQueueMax = 500 // safety cap on queued callers waiting for a slot
)

var (
	ErrUpstream    = errors.New("musicbrainz: upstream error")
	ErrNotFound    = errors.New("musicbrainz: not found")
	ErrMissingUA   = errors.New("musicbrainz: User-Agent contact is required")
)

// Client is safe for concurrent use.
type Client struct {
	httpClient *http.Client
	base       string
	userAgent  string
	limiter    *rate.Limiter
}

// Config carries construction-time options.
type Config struct {
	// Contact is the email or URL in the User-Agent. MusicBrainz rejects
	// clients without an identifiable contact.
	Contact string

	// Optional overrides.
	Base       string
	AppName    string
	Version    string
	HTTPClient *http.Client

	// RatePerSecond defaults to 1.0 — the MB hard limit. Tests may bump this.
	RatePerSecond float64
	Burst         int
}

// NewClient validates config and returns a ready client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Contact == "" {
		return nil, ErrMissingUA
	}
	base := cfg.Base
	if base == "" {
		base = DefaultBase
	}
	app := cfg.AppName
	if app == "" {
		app = defaultAppName
	}
	ver := cfg.Version
	if ver == "" {
		ver = defaultVersion
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	rps := cfg.RatePerSecond
	if rps <= 0 {
		rps = 1.0
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = 1
	}
	ua := fmt.Sprintf("%s/%s (+%s)", app, ver, cfg.Contact)
	return &Client{
		httpClient: cfg.HTTPClient,
		base:       base,
		userAgent:  ua,
		limiter:    rate.NewLimiter(rate.Limit(rps), burst),
	}, nil
}

// get issues a rate-limited GET with MB-compliant headers.
// Path must begin with "/"; query is optional.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%w: status %d (rate-limited)", ErrUpstream, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, string(body))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrUpstream, err)
	}
	return nil
}
