package releases_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/testutil"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/releases"
)

type fakeMB struct {
	byArtist map[string][]musicbrainz.ReleaseGroup
	calls    int
	err      error
}

func (f *fakeMB) BrowseReleaseGroupsByArtist(_ context.Context, artistMBID string, _ int) ([]musicbrainz.ReleaseGroup, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.byArtist[artistMBID], nil
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn := testutil.OpenTestDB(t)
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1')`); err != nil {
		t.Fatal(err)
	}
	return conn
}

func seedArtist(t *testing.T, conn *sql.DB, mbid, name string, affinity float64, followed bool) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO artists (mbid, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, mbid, name); err != nil {
		t.Fatal(err)
	}
	if followed {
		if _, err := conn.Exec(`
			INSERT INTO saved_artists (user_id, artist_mbid, saved_at)
			VALUES ('u1', $1, $2) ON CONFLICT DO NOTHING
		`, mbid, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if affinity != 0 {
		if _, err := conn.Exec(`
			INSERT INTO artist_affinity (user_id, artist_mbid, score, signal_count, updated_at)
			VALUES ('u1', $1, $2, 1, $3)
			ON CONFLICT (user_id, artist_mbid) DO UPDATE SET score = excluded.score
		`, mbid, affinity, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSync_NewReleasesForFollowedArtists(t *testing.T) {
	conn := newDB(t)
	seedArtist(t, conn, "mb-ar-1", "Artist 1", 5.0, true)

	today := time.Now().UTC().Format("2006-01-02")
	mb := &fakeMB{byArtist: map[string][]musicbrainz.ReleaseGroup{
		"mb-ar-1": {
			{MBID: "mb-rg-new", Title: "Fresh", PrimaryType: "Album", FirstReleaseDate: today},
			{MBID: "mb-rg-old", Title: "Ancient", PrimaryType: "Album", FirstReleaseDate: "2001-01-01"},
		},
	}}
	svc := releases.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok. result: %+v", r.Status, r)
	}
	if r.ArtistsScanned != 1 || r.CandidatesNew != 1 {
		t.Fatalf("counts = %+v", r)
	}

	var rows int
	conn.QueryRow(`SELECT COUNT(*) FROM discover_candidates WHERE user_id='u1' AND source='mb_new_release'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("candidates = %d, want 1", rows)
	}
	// The album row should exist for future feed lookups.
	conn.QueryRow(`SELECT COUNT(*) FROM albums WHERE mbid='mb-rg-new'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("albums = %d, want 1", rows)
	}
}

func TestSync_SkipsIfRecentRun(t *testing.T) {
	conn := newDB(t)
	seedArtist(t, conn, "mb-ar-1", "Artist 1", 5.0, true)
	if _, err := conn.Exec(`
		INSERT INTO sync_runs (user_id, kind, started_at, status)
		VALUES ('u1','mb-releases',$1,'ok')
	`, time.Now().UTC().Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	mb := &fakeMB{}
	svc := releases.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "skipped" {
		t.Fatalf("status = %q, want skipped", r.Status)
	}
	if mb.calls != 0 {
		t.Fatalf("mb.calls = %d, want 0", mb.calls)
	}
}

func TestSync_HighAffinityArtistPromotedEvenIfNotFollowed(t *testing.T) {
	conn := newDB(t)
	// Followed but ancient: won't return anything new.
	seedArtist(t, conn, "mb-ar-old", "Old Favourite", 1.0, true)
	// NOT followed but has strong affinity (e.g. top-track heavy) — should
	// still appear in the scan.
	seedArtist(t, conn, "mb-ar-rising", "Rising Star", 8.0, false)

	today := time.Now().UTC().Format("2006-01-02")
	mb := &fakeMB{byArtist: map[string][]musicbrainz.ReleaseGroup{
		"mb-ar-old":    {{MBID: "mb-rg-ancient", Title: "Old", FirstReleaseDate: "2001-01-01"}},
		"mb-ar-rising": {{MBID: "mb-rg-rising", Title: "New Drop", FirstReleaseDate: today}},
	}}
	svc := releases.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.ArtistsScanned != 2 {
		t.Fatalf("artists scanned = %d, want 2. result: %+v", r.ArtistsScanned, r)
	}
	if r.CandidatesNew != 1 {
		t.Fatalf("new = %d, want 1", r.CandidatesNew)
	}
	var who string
	conn.QueryRow(`SELECT subject_id FROM discover_candidates WHERE user_id='u1' AND source='mb_new_release'`).Scan(&who)
	if who != "mb-rg-rising" {
		t.Fatalf("candidate = %q, want mb-rg-rising", who)
	}
}

func TestSync_Idempotent_UpsertDoesNotDouble(t *testing.T) {
	conn := newDB(t)
	seedArtist(t, conn, "mb-ar-1", "A1", 5.0, true)

	today := time.Now().UTC().Format("2006-01-02")
	mb := &fakeMB{byArtist: map[string][]musicbrainz.ReleaseGroup{
		"mb-ar-1": {{MBID: "mb-rg-1", Title: "Drop", FirstReleaseDate: today}},
	}}
	svc := releases.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := svc.Sync(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	// Fast-forward by clearing the sync_runs gate; expect one candidate still.
	if _, err := conn.Exec(`DELETE FROM sync_runs`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Sync(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	var rows int
	conn.QueryRow(`SELECT COUNT(*) FROM discover_candidates WHERE user_id='u1'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("candidates = %d, want 1 (upsert, not insert)", rows)
	}
}
