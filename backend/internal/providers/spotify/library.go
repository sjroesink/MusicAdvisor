package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const pageSize = 50

// SavedAlbum describes one entry in /me/albums.
type SavedAlbum struct {
	SpotifyID    string
	Name         string
	AlbumType    string // album | single | compilation
	ReleaseDate  string
	TotalTracks  int
	DurationMs   int // summed from tracks if available
	UPC          string
	ArtistID     string // primary artist
	ArtistName   string
	AddedAt      time.Time
}

// SavedTrack describes one entry in /me/tracks.
type SavedTrack struct {
	SpotifyID   string
	Name        string
	DurationMs  int
	ISRC        string
	AlbumID     string
	AlbumName   string
	ArtistID    string
	ArtistName  string
	AddedAt     time.Time
}

// FollowedArtist describes one entry in /me/following?type=artist.
type FollowedArtist struct {
	SpotifyID string
	Name      string
}

// FetchSavedAlbums iterates /me/albums, yielding each album via onAlbum.
// Returns the number of albums seen. Errors from onAlbum propagate and stop
// iteration; HTTP errors are wrapped in ErrUpstream.
func (c *Client) FetchSavedAlbums(ctx context.Context, accessToken string, onAlbum func(SavedAlbum) error) (int, error) {
	u := c.apiBase + "/me/albums?limit=" + strconv.Itoa(pageSize)
	total := 0
	for u != "" {
		var page struct {
			Next  string `json:"next"`
			Items []struct {
				AddedAt string `json:"added_at"`
				Album   struct {
					ID              string `json:"id"`
					Name            string `json:"name"`
					AlbumType       string `json:"album_type"`
					ReleaseDate     string `json:"release_date"`
					TotalTracks     int    `json:"total_tracks"`
					ExternalIDs struct {
						UPC string `json:"upc"`
					} `json:"external_ids"`
					Artists []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"artists"`
				} `json:"album"`
			} `json:"items"`
		}
		if err := c.getJSON(ctx, u, accessToken, &page); err != nil {
			return total, err
		}
		for _, it := range page.Items {
			a := it.Album
			sa := SavedAlbum{
				SpotifyID:   a.ID,
				Name:        a.Name,
				AlbumType:   a.AlbumType,
				ReleaseDate: a.ReleaseDate,
				TotalTracks: a.TotalTracks,
				UPC:         a.ExternalIDs.UPC,
				AddedAt:     parseTimeRFC3339(it.AddedAt),
			}
			if len(a.Artists) > 0 {
				sa.ArtistID = a.Artists[0].ID
				sa.ArtistName = a.Artists[0].Name
			}
			if err := onAlbum(sa); err != nil {
				return total, err
			}
			total++
		}
		u = page.Next
	}
	return total, nil
}

// FetchSavedTracks iterates /me/tracks.
func (c *Client) FetchSavedTracks(ctx context.Context, accessToken string, onTrack func(SavedTrack) error) (int, error) {
	u := c.apiBase + "/me/tracks?limit=" + strconv.Itoa(pageSize)
	total := 0
	for u != "" {
		var page struct {
			Next  string `json:"next"`
			Items []struct {
				AddedAt string `json:"added_at"`
				Track   struct {
					ID          string `json:"id"`
					Name        string `json:"name"`
					DurationMs  int    `json:"duration_ms"`
					ExternalIDs struct {
						ISRC string `json:"isrc"`
					} `json:"external_ids"`
					Album struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"album"`
					Artists []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"artists"`
				} `json:"track"`
			} `json:"items"`
		}
		if err := c.getJSON(ctx, u, accessToken, &page); err != nil {
			return total, err
		}
		for _, it := range page.Items {
			t := it.Track
			st := SavedTrack{
				SpotifyID:  t.ID,
				Name:       t.Name,
				DurationMs: t.DurationMs,
				ISRC:       t.ExternalIDs.ISRC,
				AlbumID:    t.Album.ID,
				AlbumName:  t.Album.Name,
				AddedAt:    parseTimeRFC3339(it.AddedAt),
			}
			if len(t.Artists) > 0 {
				st.ArtistID = t.Artists[0].ID
				st.ArtistName = t.Artists[0].Name
			}
			if err := onTrack(st); err != nil {
				return total, err
			}
			total++
		}
		u = page.Next
	}
	return total, nil
}

// FetchFollowedArtists iterates /me/following?type=artist. This endpoint uses
// an "after" cursor shaped differently from the standard next-URL flow, so
// we hand-walk the cursor.
func (c *Client) FetchFollowedArtists(ctx context.Context, accessToken string, onArtist func(FollowedArtist) error) (int, error) {
	q := url.Values{
		"type":  {"artist"},
		"limit": {strconv.Itoa(pageSize)},
	}
	base := c.apiBase + "/me/following"
	total := 0
	for {
		u := base + "?" + q.Encode()
		var page struct {
			Artists struct {
				Next   string `json:"next"`
				Cursors struct {
					After string `json:"after"`
				} `json:"cursors"`
				Items []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"items"`
			} `json:"artists"`
		}
		if err := c.getJSON(ctx, u, accessToken, &page); err != nil {
			return total, err
		}
		for _, it := range page.Artists.Items {
			if err := onArtist(FollowedArtist{SpotifyID: it.ID, Name: it.Name}); err != nil {
				return total, err
			}
			total++
		}
		if page.Artists.Next == "" || page.Artists.Cursors.After == "" {
			return total, nil
		}
		q.Set("after", page.Artists.Cursors.After)
	}
}

// getJSON is the shared authenticated-GET helper for library endpoints.
func (c *Client) getJSON(ctx context.Context, url, accessToken string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%w: status %d: %s", ErrUpstream, resp.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

func parseTimeRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
