package musicbrainz_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
)

func newTestClient(t *testing.T, handler http.Handler, rps float64) *musicbrainz.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := musicbrainz.NewClient(musicbrainz.Config{
		Contact:       "test@example.com",
		Base:          srv.URL,
		RatePerSecond: rps,
		Burst:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClient_RequiresContact(t *testing.T) {
	_, err := musicbrainz.NewClient(musicbrainz.Config{})
	if !errors.Is(err, musicbrainz.ErrMissingUA) {
		t.Fatalf("err = %v, want ErrMissingUA", err)
	}
}

func TestLookupTrackByISRC_OK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/recording", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != "isrc:USABC1234567" {
			t.Errorf("query = %q", got)
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "MusicAdvisor") {
			t.Errorf("User-Agent = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"recordings":[{
				"id":"rec-mbid","title":"Spooky","length":240000,
				"artist-credit":[{"artist":{"id":"artist-mbid","name":"Grouper"}}],
				"releases":[{"release-group":{"id":"rg-mbid"}}]
			}]
		}`))
	})
	c := newTestClient(t, mux, 100)
	got, err := c.LookupTrackByISRC(context.Background(), "USABC1234567")
	if err != nil {
		t.Fatal(err)
	}
	if got.MBID != "rec-mbid" || got.ArtistID != "artist-mbid" ||
		got.ReleaseGroupID != "rg-mbid" || got.Title != "Spooky" {
		t.Fatalf("got %+v", got)
	}
}

func TestLookupTrackByISRC_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/recording", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recordings":[]}`))
	})
	c := newTestClient(t, mux, 100)
	_, err := c.LookupTrackByISRC(context.Background(), "notreal")
	if !errors.Is(err, musicbrainz.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLookupAlbumByUPC_OK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"releases":[{
				"release-group":{"id":"rg-1","title":"Paraphrases",
					"primary-type":"Album","first-release-date":"2026-04-18"},
				"artist-credit":[{"artist":{"id":"a-1","name":"Nils Frahm"}}]
			}]
		}`))
	})
	c := newTestClient(t, mux, 100)
	got, err := c.LookupAlbumByUPC(context.Background(), "0123456789012")
	if err != nil {
		t.Fatal(err)
	}
	if got.MBID != "rg-1" || got.PrimaryType != "Album" ||
		got.ArtistName != "Nils Frahm" || got.FirstReleaseDate != "2026-04-18" {
		t.Fatalf("got %+v", got)
	}
}

func TestSearchArtistByName_ScoreFlows(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/artist", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artists":[{"id":"a-1","name":"Nils Frahm","sort-name":"Frahm, Nils","score":100}]}`))
	})
	c := newTestClient(t, mux, 100)
	got, err := c.SearchArtistByName(context.Background(), "Nils Frahm")
	if err != nil {
		t.Fatal(err)
	}
	if got.Score != 100 {
		t.Fatalf("score = %d, want 100", got.Score)
	}
}

func TestRateLimiter_SerializesConcurrentCalls(t *testing.T) {
	// Rate 5/s burst 1 → two back-to-back calls must be ≥ 200ms apart.
	var firstAt, secondAt time.Time
	var mu sync.Mutex
	n := 0

	mux := http.NewServeMux()
	mux.HandleFunc("/recording", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		switch n {
		case 1:
			firstAt = time.Now()
		case 2:
			secondAt = time.Now()
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"recordings":[]}`))
	})
	c := newTestClient(t, mux, 5.0) // 5 per second, burst 1

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.LookupTrackByISRC(context.Background(), "x")
		}()
	}
	wg.Wait()

	gap := secondAt.Sub(firstAt)
	if gap < 150*time.Millisecond {
		t.Fatalf("second call arrived after only %s; rate limiter not enforcing", gap)
	}
}
