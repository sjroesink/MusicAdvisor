package listenbrainz_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/providers/listenbrainz"
)

func TestFetchSimilarArtists_FlattensDatasets(t *testing.T) {
	mux := http.NewServeMux()
	var gotArtist string
	mux.HandleFunc("/similar-artists/json", func(w http.ResponseWriter, r *http.Request) {
		gotArtist = r.URL.Query().Get("artist_mbids")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
		 {"data":[
		   {"artist_mbid":"mb-a","name":"A","score":0.9},
		   {"artist_mbid":"mb-src","name":"Source","score":1.0},
		   {"artist_mbid":"mb-b","name":"B","score":0.7}
		 ]}
		]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := listenbrainz.NewClient(listenbrainz.Config{
		Contact: "test@example.com",
		Base:    srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.FetchSimilarArtists(context.Background(), "mb-src", 10)
	if err != nil {
		t.Fatal(err)
	}
	if gotArtist != "mb-src" {
		t.Fatalf("artist_mbids sent = %q", gotArtist)
	}
	// mb-src itself should be filtered out.
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2. got: %+v", len(got), got)
	}
	if got[0].MBID != "mb-a" || got[0].Score != 0.9 || got[1].MBID != "mb-b" {
		t.Fatalf("got = %+v", got)
	}
}

func TestNewClient_RequiresContact(t *testing.T) {
	if _, err := listenbrainz.NewClient(listenbrainz.Config{}); err == nil {
		t.Fatal("expected ErrMissingUA")
	}
}
