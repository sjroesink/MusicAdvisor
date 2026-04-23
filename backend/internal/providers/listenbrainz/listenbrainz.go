// Package listenbrainz is a narrow client for the ListenBrainz labs
// similar-artists endpoint. ListenBrainz doesn't require auth for this
// call but does expect a User-Agent contact like MB does.
//
// The labs endpoint returns a nested array shape (a list of "datasets"
// each containing a list of similar artists). We flatten to a single
// []SimilarArtist for callers.
package listenbrainz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	DefaultBase = "https://labs.api.listenbrainz.org"
	// DefaultAlgorithm is a session-based similar-artists model that's a
	// reasonable default for most libraries. LB exposes many variants; if
	// recall is low this is the first knob to turn.
	DefaultAlgorithm = "session_based_days_7500_session_300_contribution_5_threshold_10_limit_100_filter_True_skip_30"
)

var (
	ErrUpstream  = errors.New("listenbrainz: upstream error")
	ErrMissingUA = errors.New("listenbrainz: User-Agent contact is required")
)

type Client struct {
	httpClient *http.Client
	base       string
	userAgent  string
	algorithm  string
}

type Config struct {
	Contact    string // required, goes into User-Agent
	Base       string
	Algorithm  string
	HTTPClient *http.Client
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.Contact == "" {
		return nil, ErrMissingUA
	}
	base := cfg.Base
	if base == "" {
		base = DefaultBase
	}
	alg := cfg.Algorithm
	if alg == "" {
		alg = DefaultAlgorithm
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		httpClient: cfg.HTTPClient,
		base:       base,
		userAgent:  "MusicAdvisor/0.1 (+" + cfg.Contact + ")",
		algorithm:  alg,
	}, nil
}

// SimilarArtist is one entry returned by the labs endpoint.
type SimilarArtist struct {
	MBID  string
	Name  string
	Score float64
}

// FetchSimilarArtists queries /similar-artists/json and returns the
// flattened list. LB caps server-side at the algorithm's "limit" parameter
// (100 by default); the additional limit here is client-side.
func (c *Client) FetchSimilarArtists(ctx context.Context, artistMBID string, limit int) ([]SimilarArtist, error) {
	if limit <= 0 {
		limit = 50
	}
	q := url.Values{
		"artist_mbids": {artistMBID},
		"algorithm":    {c.algorithm},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/similar-artists/json?"+q.Encode(), nil)
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
	// Labs response: list of datasets, each with data[]. We flatten.
	var datasets []struct {
		Data []struct {
			ArtistMBID string  `json:"artist_mbid"`
			Name       string  `json:"name"`
			Score      float64 `json:"score"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &datasets); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUpstream, err)
	}
	out := make([]SimilarArtist, 0, 16)
	for _, ds := range datasets {
		for _, d := range ds.Data {
			if d.ArtistMBID == "" || d.ArtistMBID == artistMBID {
				continue
			}
			out = append(out, SimilarArtist{MBID: d.ArtistMBID, Name: d.Name, Score: d.Score})
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}
