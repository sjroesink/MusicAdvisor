package handlers_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/http/handlers"
)

// authedFeed bootstraps a session via the Spotify round-trip and returns
// the harness plus the actual UUID user_id that got created by the
// callback (user IDs are random UUIDs, not predictable from the external
// handle).
func authedFeed(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t, true)

	resp, _ := h.client.Get(h.server.URL + "/api/auth/spotify/login")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp.Body.Close()
	state := loc.Query().Get("state")
	cb, _ := h.client.Get(h.server.URL + "/api/auth/spotify/callback?code=c&state=" + state)
	cb.Body.Close()

	var userID string
	if err := h.DB().QueryRow(`
		SELECT user_id FROM external_accounts WHERE provider = 'spotify' AND external_id = 'sander'
	`).Scan(&userID); err != nil {
		t.Fatalf("lookup user_id: %v", err)
	}
	return h, userID
}

func TestFeed_Unauthenticated401(t *testing.T) {
	h := newHarness(t, false)
	resp, err := h.client.Get(h.server.URL + "/api/feed")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestFeed_EmptyLibrary_ReturnsIdleHeader(t *testing.T) {
	h, _ := authedFeed(t)
	resp, err := h.client.Get(h.server.URL + "/api/feed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, string(b))
	}
	var got handlers.FeedResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Header.Status != "idle" {
		t.Fatalf("status = %q, want idle", got.Header.Status)
	}
	if got.Header.LibraryCount != 0 {
		t.Fatalf("libraryCount = %d, want 0", got.Header.LibraryCount)
	}
	if len(got.NewReleases) != 0 || len(got.Discover) != 0 {
		t.Fatalf("expected empty cards, got %+v / %+v", got.NewReleases, got.Discover)
	}
}

func TestFeed_SurfacesCandidatesInScoreOrder(t *testing.T) {
	h, userID := authedFeed(t)

	// Seed artist + album + two candidates (one per source) with known scores.
	db := h.DB()
	db.Exec(`INSERT INTO artists (mbid, name) VALUES ('ar-1','Seed Artist')`)
	db.Exec(`INSERT INTO albums (mbid, primary_artist_mbid, title, type, release_date)
	         VALUES ('al-1','ar-1','Big Drop','Album','2026-04-10')`)
	db.Exec(`INSERT INTO albums (mbid, primary_artist_mbid, title, type, release_date)
	         VALUES ('al-2','ar-1','Side Drop','EP','2026-04-05')`)
	db.Exec(`INSERT INTO discover_candidates
	         (user_id, subject_type, subject_id, source, raw_score, reason_data, expires_at)
	         VALUES (?, 'album', 'al-1', 'mb_new_release', 0.9,
	                 '{"via_artist_name":"Seed Artist","primary_type":"Album"}',
	                 ?)`, userID, time.Now().Add(time.Hour))
	db.Exec(`INSERT INTO discover_candidates
	         (user_id, subject_type, subject_id, source, raw_score, reason_data, expires_at)
	         VALUES (?, 'album', 'al-2', 'listenbrainz', 0.7,
	                 '{"via_artist_name":"Other","lb_score":0.7}',
	                 ?)`, userID, time.Now().Add(time.Hour))

	resp, _ := h.client.Get(h.server.URL + "/api/feed")
	defer resp.Body.Close()
	var got handlers.FeedResponse
	json.NewDecoder(resp.Body).Decode(&got)

	if len(got.NewReleases) != 1 {
		t.Fatalf("new_releases = %d, want 1", len(got.NewReleases))
	}
	if got.NewReleases[0].ID != "al-1" || got.NewReleases[0].Artist != "Seed Artist" {
		t.Fatalf("new_releases[0] = %+v", got.NewReleases[0])
	}
	if got.NewReleases[0].Year != 2026 || got.NewReleases[0].Date == "" {
		t.Fatalf("date/year formatting: %+v", got.NewReleases[0])
	}
	if len(got.Discover) != 1 || got.Discover[0].ID != "al-2" {
		t.Fatalf("discover = %+v", got.Discover)
	}
}

func TestFeed_MergesDuplicateDiscoverSources(t *testing.T) {
	h, userID := authedFeed(t)
	db := h.DB()
	db.Exec(`INSERT INTO artists (mbid, name) VALUES ('ar-d','Dup Artist')`)
	db.Exec(`INSERT INTO albums (mbid, primary_artist_mbid, title, type, release_date)
	         VALUES ('al-dup','ar-d','Shared','Album','2026-04-10')`)
	// Same release hits via lb_similar (high score), mb_artist_rels (mid),
	// and lastfm_similar (low). Should collapse into one card with all
	// three sources listed.
	db.Exec(`INSERT INTO discover_candidates
	         (user_id, subject_type, subject_id, source, raw_score, reason_data, expires_at)
	         VALUES (?, 'album', 'al-dup', 'listenbrainz', 0.9, '{"via_artist_name":"LB Seed"}', ?)`,
		userID, time.Now().Add(time.Hour))
	db.Exec(`INSERT INTO discover_candidates
	         (user_id, subject_type, subject_id, source, raw_score, reason_data, expires_at)
	         VALUES (?, 'album', 'al-dup', 'mb_artist_rels', 0.6, '{"via_artist_name":"MB Seed","relation":"collaboration"}', ?)`,
		userID, time.Now().Add(time.Hour))
	db.Exec(`INSERT INTO discover_candidates
	         (user_id, subject_type, subject_id, source, raw_score, reason_data, expires_at)
	         VALUES (?, 'album', 'al-dup', 'lastfm_similar', 0.4, '{"via_artist_name":"LF Seed"}', ?)`,
		userID, time.Now().Add(time.Hour))

	resp, _ := h.client.Get(h.server.URL + "/api/feed")
	defer resp.Body.Close()
	var got handlers.FeedResponse
	json.NewDecoder(resp.Body).Decode(&got)

	if len(got.Discover) != 1 {
		t.Fatalf("discover = %d, want 1 (deduped). cards=%+v", len(got.Discover), got.Discover)
	}
	card := got.Discover[0]
	if card.ID != "al-dup" {
		t.Fatalf("id = %q, want al-dup", card.ID)
	}
	if card.Source != "listenbrainz" {
		t.Fatalf("primary source = %q, want listenbrainz (highest score)", card.Source)
	}
	if len(card.Sources) != 3 {
		t.Fatalf("sources = %v, want 3 entries", card.Sources)
	}
}

func TestFeed_ExcludesExpiredCandidates(t *testing.T) {
	h, userID := authedFeed(t)
	db := h.DB()
	db.Exec(`INSERT INTO artists (mbid, name) VALUES ('ar-x','X')`)
	db.Exec(`INSERT INTO albums (mbid, primary_artist_mbid, title, type) VALUES ('al-x','ar-x','T','Album')`)
	db.Exec(`INSERT INTO discover_candidates
	         (user_id, subject_type, subject_id, source, raw_score, reason_data, expires_at)
	         VALUES (?, 'album', 'al-x', 'mb_new_release', 0.5, '{}', ?)`,
		userID, time.Now().Add(-1*time.Hour))

	resp, _ := h.client.Get(h.server.URL + "/api/feed")
	defer resp.Body.Close()
	var got handlers.FeedResponse
	json.NewDecoder(resp.Body).Decode(&got)
	if len(got.NewReleases) != 0 {
		t.Fatalf("expected 0 new_releases (expired), got %d", len(got.NewReleases))
	}
}
