package spotify_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
)

func TestFetchSavedAlbums_Paginates(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/me/albums", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Fabricate two pages: first returns next URL pointing to ?offset=2
		if r.URL.Query().Get("offset") == "" {
			next := fmt.Sprintf("%s/me/albums?limit=50&offset=2", srv.URL)
			fmt.Fprintf(w, `{
				"next": %q,
				"items": [
					{"added_at":"2026-01-01T00:00:00Z","album":{"id":"a1","name":"First","album_type":"album","release_date":"2026-01-01","total_tracks":10,"external_ids":{"upc":"111"},"artists":[{"id":"ar1","name":"Artist One"}]}},
					{"added_at":"2026-01-02T00:00:00Z","album":{"id":"a2","name":"Second","album_type":"album","release_date":"2026-01-02","total_tracks":9,"external_ids":{"upc":"222"},"artists":[{"id":"ar2","name":"Artist Two"}]}}
				]
			}`, next)
			return
		}
		fmt.Fprint(w, `{
			"next": null,
			"items": [
				{"added_at":"2026-01-03T00:00:00Z","album":{"id":"a3","name":"Third","album_type":"ep","release_date":"2026-01-03","total_tracks":5,"external_ids":{"upc":"333"},"artists":[{"id":"ar3","name":"Artist Three"}]}}
			]
		}`)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := spotify.NewClient(spotify.Config{
		ClientID: "id", ClientSecret: "secret",
		RedirectURI: "http://x",
		APIBase:     srv.URL,
		AuthBase:    srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	var got []spotify.SavedAlbum
	n, err := c.FetchSavedAlbums(context.Background(), "tok", func(a spotify.SavedAlbum) error {
		got = append(got, a)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("n = %d, want 3", n)
	}
	if got[0].SpotifyID != "a1" || got[1].ArtistID != "ar2" || got[2].UPC != "333" {
		t.Fatalf("got %+v", got)
	}
}

func TestFetchSavedTracks_CapturesISRC(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/me/tracks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"next": null,
			"items": [
				{"added_at":"2026-01-01T00:00:00Z","track":{"id":"t1","name":"Spooky","duration_ms":240000,"external_ids":{"isrc":"USABC"},"album":{"id":"al1","name":"Album"},"artists":[{"id":"ar1","name":"Grouper"}]}}
			]
		}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, _ := spotify.NewClient(spotify.Config{
		ClientID: "id", ClientSecret: "secret", RedirectURI: "http://x", APIBase: srv.URL, AuthBase: srv.URL,
	})

	var got spotify.SavedTrack
	_, err := c.FetchSavedTracks(context.Background(), "tok", func(t spotify.SavedTrack) error {
		got = t
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ISRC != "USABC" || got.SpotifyID != "t1" || got.AlbumID != "al1" || got.ArtistName != "Grouper" {
		t.Fatalf("got %+v", got)
	}
}

func TestFetchTopArtists_ReturnsRankedSlice(t *testing.T) {
	mux := http.NewServeMux()
	var gotRange string
	mux.HandleFunc("/me/top/artists", func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.URL.Query().Get("time_range")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[{"id":"ar1","name":"First"},{"id":"ar2","name":"Second"},{"id":"ar3","name":"Third"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, _ := spotify.NewClient(spotify.Config{
		ClientID: "id", ClientSecret: "secret", RedirectURI: "http://x", APIBase: srv.URL, AuthBase: srv.URL,
	})

	got, err := c.FetchTopArtists(context.Background(), "tok", spotify.TopRangeMedium)
	if err != nil {
		t.Fatal(err)
	}
	if gotRange != "medium_term" {
		t.Fatalf("time_range sent = %q, want medium_term", gotRange)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	if got[0].Rank != 1 || got[2].Rank != 3 {
		t.Fatalf("ranks not 1-based: %+v", got)
	}
	if got[0].SpotifyID != "ar1" || got[1].Name != "Second" {
		t.Fatalf("got[0..1] = %+v", got[:2])
	}
}

func TestFetchTopTracks_CapturesISRCAndRank(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/me/top/tracks", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("time_range") != "short_term" {
			t.Errorf("time_range = %q", r.URL.Query().Get("time_range"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[
			{"id":"t1","name":"One","duration_ms":240000,"external_ids":{"isrc":"US-A"},"album":{"id":"al1","name":"Album"},"artists":[{"id":"ar1","name":"A"}]},
			{"id":"t2","name":"Two","duration_ms":180000,"external_ids":{"isrc":"US-B"},"album":{"id":"al2","name":"Album 2"},"artists":[{"id":"ar2","name":"B"}]}
		]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, _ := spotify.NewClient(spotify.Config{
		ClientID: "id", ClientSecret: "secret", RedirectURI: "http://x", APIBase: srv.URL, AuthBase: srv.URL,
	})

	got, err := c.FetchTopTracks(context.Background(), "tok", spotify.TopRangeShort)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d", len(got))
	}
	if got[0].Rank != 1 || got[1].Rank != 2 {
		t.Fatalf("ranks = %d,%d", got[0].Rank, got[1].Rank)
	}
	if got[0].ISRC != "US-A" || got[0].DurationMs != 240000 || got[0].ArtistID != "ar1" {
		t.Fatalf("got[0] = %+v", got[0])
	}
}

func TestFetchRecentlyPlayed_ParsesPlayedAtAndContext(t *testing.T) {
	mux := http.NewServeMux()
	var gotAfter string
	mux.HandleFunc("/me/player/recently-played", func(w http.ResponseWriter, r *http.Request) {
		gotAfter = r.URL.Query().Get("after")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"items":[
			{"played_at":"2026-04-23T12:00:00Z","context":{"uri":"spotify:album:al1"},
			 "track":{"id":"t1","name":"First","duration_ms":200000,"external_ids":{"isrc":"US-X"},
			          "album":{"id":"al1","name":"Album"},"artists":[{"id":"ar1","name":"A"}]}},
			{"played_at":"2026-04-23T11:55:00Z","context":{"uri":""},
			 "track":{"id":"t2","name":"Second","duration_ms":240000,"external_ids":{"isrc":"US-Y"},
			          "album":{"id":"al2","name":"Album 2"},"artists":[{"id":"ar2","name":"B"}]}}
		]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, _ := spotify.NewClient(spotify.Config{
		ClientID: "id", ClientSecret: "secret", RedirectURI: "http://x", APIBase: srv.URL, AuthBase: srv.URL,
	})

	cutoff := time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC)
	got, err := c.FetchRecentlyPlayed(context.Background(), "tok", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if gotAfter == "" {
		t.Fatalf("after cursor not sent")
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d", len(got))
	}
	if got[0].SpotifyID != "t1" || got[0].ContextURI != "spotify:album:al1" {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[0].PlayedAt.IsZero() {
		t.Fatalf("played_at not parsed: %+v", got[0])
	}
}

func TestFetchFollowedArtists_CursorPagination(t *testing.T) {
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/me/following", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		after := r.URL.Query().Get("after")
		switch after {
		case "":
			fmt.Fprint(w, `{"artists":{"next":"/me/following?after=cursor1","cursors":{"after":"cursor1"},"items":[{"id":"ar1","name":"A1"}]}}`)
		case "cursor1":
			fmt.Fprint(w, `{"artists":{"next":"","cursors":{"after":""},"items":[{"id":"ar2","name":"A2"}]}}`)
		default:
			t.Errorf("unexpected cursor %q", after)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, _ := spotify.NewClient(spotify.Config{
		ClientID: "id", ClientSecret: "secret", RedirectURI: "http://x", APIBase: srv.URL, AuthBase: srv.URL,
	})

	var seen []string
	n, err := c.FetchFollowedArtists(context.Background(), "tok", func(a spotify.FollowedArtist) error {
		seen = append(seen, a.SpotifyID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(seen) != 2 || seen[0] != "ar1" || seen[1] != "ar2" {
		t.Fatalf("seen = %v", seen)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}
