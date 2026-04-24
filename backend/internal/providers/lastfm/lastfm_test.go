package lastfm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/providers/lastfm"
)

func TestFetchSimilarArtists_ParsesMatchAsString(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "k" {
			t.Errorf("missing api_key")
		}
		if r.URL.Query().Get("mbid") != "seed-mbid" {
			t.Errorf("missing mbid: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"similarartists": {
				"artist": [
					{"name":"Daughter","mbid":"dau-mbid","match":"0.845"},
					{"name":"Nameless","mbid":"","match":"0.61"}
				]
			}
		}`))
	}))
	defer ts.Close()

	c, err := lastfm.NewClient(lastfm.Config{APIKey: "k", Base: ts.URL, RatePerSecond: 1000})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.FetchSimilarArtists(context.Background(), "seed-mbid", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2. got=%+v", len(got), got)
	}
	if got[0].Name != "Daughter" || got[0].Score < 0.84 || got[0].Score > 0.85 {
		t.Fatalf("first = %+v", got[0])
	}
	if got[1].MBID != "" {
		t.Fatalf("second mbid = %q, want empty passthrough", got[1].MBID)
	}
}

func TestFetchSimilarArtists_SurfacesAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":10,"message":"Invalid API key"}`))
	}))
	defer ts.Close()

	c, err := lastfm.NewClient(lastfm.Config{APIKey: "k", Base: ts.URL, RatePerSecond: 1000})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.FetchSimilarArtists(context.Background(), "x", "", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "Invalid API key") {
		t.Fatalf("err = %v", err)
	}
}
