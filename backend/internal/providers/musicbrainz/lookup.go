package musicbrainz

import (
	"context"
	"net/url"
)

// Track is the subset of a MusicBrainz recording we care about when
// resolving a Spotify track to an MBID.
type Track struct {
	MBID     string
	Title    string
	Length   int // milliseconds (optional)
	ArtistID string
	ArtistName string
	ReleaseGroupID string // first related release-group, best-effort
}

// LookupTrackByISRC resolves a Spotify track (via its ISRC) to a MusicBrainz
// recording. Returns ErrNotFound when no match exists. Confidence semantics
// are the caller's — the resolver uses "ISRC hit = confidence 0.95".
func (c *Client) LookupTrackByISRC(ctx context.Context, isrc string) (Track, error) {
	q := url.Values{
		"query": {"isrc:" + isrc},
		"fmt":   {"json"},
		"limit": {"1"},
	}
	var body struct {
		Recordings []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Length int    `json:"length"`
			ArtistCredit []struct {
				Artist struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"artist"`
			} `json:"artist-credit"`
			Releases []struct {
				ReleaseGroup struct {
					ID string `json:"id"`
				} `json:"release-group"`
			} `json:"releases"`
		} `json:"recordings"`
	}
	if err := c.get(ctx, "/recording", q, &body); err != nil {
		return Track{}, err
	}
	if len(body.Recordings) == 0 {
		return Track{}, ErrNotFound
	}
	r := body.Recordings[0]
	t := Track{MBID: r.ID, Title: r.Title, Length: r.Length}
	if len(r.ArtistCredit) > 0 {
		t.ArtistID = r.ArtistCredit[0].Artist.ID
		t.ArtistName = r.ArtistCredit[0].Artist.Name
	}
	if len(r.Releases) > 0 {
		t.ReleaseGroupID = r.Releases[0].ReleaseGroup.ID
	}
	return t, nil
}

// Album is a MusicBrainz release-group summary.
type Album struct {
	MBID       string
	Title      string
	ArtistID   string
	ArtistName string
	PrimaryType string // "Album" | "EP" | "Single"
	FirstReleaseDate string
}

// LookupAlbumByUPC resolves a Spotify album (via its UPC barcode) to a
// MusicBrainz release-group.
func (c *Client) LookupAlbumByUPC(ctx context.Context, upc string) (Album, error) {
	q := url.Values{
		"query": {"barcode:" + upc},
		"fmt":   {"json"},
		"limit": {"1"},
	}
	var body struct {
		Releases []struct {
			ReleaseGroup struct {
				ID              string `json:"id"`
				Title           string `json:"title"`
				PrimaryType     string `json:"primary-type"`
				FirstReleaseDate string `json:"first-release-date"`
			} `json:"release-group"`
			ArtistCredit []struct {
				Artist struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"artist"`
			} `json:"artist-credit"`
		} `json:"releases"`
	}
	if err := c.get(ctx, "/release", q, &body); err != nil {
		return Album{}, err
	}
	if len(body.Releases) == 0 {
		return Album{}, ErrNotFound
	}
	r := body.Releases[0]
	a := Album{
		MBID:             r.ReleaseGroup.ID,
		Title:            r.ReleaseGroup.Title,
		PrimaryType:      r.ReleaseGroup.PrimaryType,
		FirstReleaseDate: r.ReleaseGroup.FirstReleaseDate,
	}
	if len(r.ArtistCredit) > 0 {
		a.ArtistID = r.ArtistCredit[0].Artist.ID
		a.ArtistName = r.ArtistCredit[0].Artist.Name
	}
	return a, nil
}

// Artist is a MusicBrainz artist summary.
type Artist struct {
	MBID     string
	Name     string
	SortName string
	Score    int // MusicBrainz-returned match score 0..100
}

// SearchArtistByName returns the top MB candidate for a given name. Tight
// string matching is out of scope for MVP; callers should treat results
// with score < 90 as ambiguous and tombstone them if certainty matters.
func (c *Client) SearchArtistByName(ctx context.Context, name string) (Artist, error) {
	q := url.Values{
		"query": {"artist:\"" + name + "\""},
		"fmt":   {"json"},
		"limit": {"1"},
	}
	var body struct {
		Artists []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			SortName string `json:"sort-name"`
			Score    int    `json:"score"`
		} `json:"artists"`
	}
	if err := c.get(ctx, "/artist", q, &body); err != nil {
		return Artist{}, err
	}
	if len(body.Artists) == 0 {
		return Artist{}, ErrNotFound
	}
	a := body.Artists[0]
	return Artist{MBID: a.ID, Name: a.Name, SortName: a.SortName, Score: a.Score}, nil
}
