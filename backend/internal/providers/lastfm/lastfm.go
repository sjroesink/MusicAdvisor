// Package lastfm is a narrow client for the Last.fm web API. For now it
// only implements the single call the discover pipeline needs:
// artist.getSimilar. Per-user calls (user.getLovedTracks, user.getRecentTracks)
// require OAuth and are deferred to a later phase.
package lastfm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

const (
	DefaultBase = "https://ws.audioscrobbler.com/2.0/"
	// DefaultRPS is well under Last.fm's 5 rps documented ceiling. If a user
	// has many seed artists we'd rather be slow than get the API key banned.
	DefaultRPS = 3.0
)

var (
	ErrUpstream   = errors.New("lastfm: upstream error")
	ErrMissingKey = errors.New("lastfm: API key is required")
	ErrAPI        = errors.New("lastfm: API error")
)

type Client struct {
	httpClient *http.Client
	base       string
	apiKey     string
	userAgent  string
	limiter    *rate.Limiter
}

type Config struct {
	APIKey        string
	Contact       string // goes into User-Agent; required for good-citizen reasons
	Base          string
	HTTPClient    *http.Client
	RatePerSecond float64
	Burst         int
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, ErrMissingKey
	}
	base := cfg.Base
	if base == "" {
		base = DefaultBase
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	rps := cfg.RatePerSecond
	if rps <= 0 {
		rps = DefaultRPS
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = 1
	}
	ua := "MusicAdvisor/0.1"
	if cfg.Contact != "" {
		ua += " (+" + cfg.Contact + ")"
	}
	return &Client{
		httpClient: cfg.HTTPClient,
		base:       base,
		apiKey:     cfg.APIKey,
		userAgent:  ua,
		limiter:    rate.NewLimiter(rate.Limit(rps), burst),
	}, nil
}

// SimilarArtist mirrors listenbrainz.SimilarArtist so callers can swap
// providers without changing field names. Score is Last.fm's `match`
// field (0..1). MBID may be empty when Last.fm has no MB cross-reference;
// callers must handle that by resolving via name.
type SimilarArtist struct {
	MBID  string
	Name  string
	Score float64
}

// FetchSimilarArtists calls `artist.getSimilar` and returns up to `limit`
// similar artists. Query-by-MBID when we have one, else by name.
func (c *Client) FetchSimilarArtists(ctx context.Context, seedMBID, seedName string, limit int) ([]SimilarArtist, error) {
	if seedMBID == "" && seedName == "" {
		return nil, errors.New("lastfm: seedMBID or seedName required")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	q := url.Values{
		"method":         {"artist.getSimilar"},
		"api_key":        {c.apiKey},
		"format":         {"json"},
		"limit":          {fmt.Sprintf("%d", limit)},
		"autocorrect":    {"1"},
	}
	if seedMBID != "" {
		q.Set("mbid", seedMBID)
	} else {
		q.Set("artist", seedName)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, string(body))
	}

	// Last.fm returns 200 even on method errors; the body carries
	// {"error":N,"message":"..."} in that case.
	var probe struct {
		Error   int    `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Error != 0 {
		return nil, fmt.Errorf("%w: %d: %s", ErrAPI, probe.Error, probe.Message)
	}

	var parsed struct {
		Similar struct {
			Artists []struct {
				Name  string `json:"name"`
				MBID  string `json:"mbid"`
				Match any    `json:"match"` // Last.fm returns match as a string in JSON
			} `json:"artist"`
		} `json:"similarartists"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUpstream, err)
	}

	out := make([]SimilarArtist, 0, len(parsed.Similar.Artists))
	for _, a := range parsed.Similar.Artists {
		if a.MBID == seedMBID && a.MBID != "" {
			continue
		}
		score, _ := parseMatch(a.Match)
		out = append(out, SimilarArtist{
			MBID:  a.MBID,
			Name:  a.Name,
			Score: score,
		})
	}
	return out, nil
}

// parseMatch accepts string or float for the `match` field — older
// versions of the API emitted one, newer versions the other.
func parseMatch(v any) (float64, error) {
	switch x := v.(type) {
	case string:
		return strconv.ParseFloat(x, 64)
	case float64:
		return x, nil
	default:
		return 0, nil
	}
}
