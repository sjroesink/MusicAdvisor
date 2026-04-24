package mbrels_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/db"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/mbrels"
)

type fakeMB struct {
	rels     map[string][]musicbrainz.ArtistRelation
	byArtist map[string][]musicbrainz.ReleaseGroup
	relErr   error
	browseErr error
}

func (f *fakeMB) FetchArtistRelations(_ context.Context, mbid string) ([]musicbrainz.ArtistRelation, error) {
	if f.relErr != nil {
		return nil, f.relErr
	}
	return f.rels[mbid], nil
}

func (f *fakeMB) BrowseReleaseGroupsByArtist(_ context.Context, mbid string, _ int) ([]musicbrainz.ReleaseGroup, error) {
	if f.browseErr != nil {
		return nil, f.browseErr
	}
	return f.byArtist[mbid], nil
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1')`); err != nil {
		t.Fatal(err)
	}
	return conn
}

func seedFollowed(t *testing.T, conn *sql.DB, mbid, name string, aff float64) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO artists (mbid, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, mbid, name); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		INSERT INTO saved_artists (user_id, artist_mbid, saved_at)
		VALUES ('u1', ?, ?) ON CONFLICT DO NOTHING
	`, mbid, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		INSERT INTO artist_affinity (user_id, artist_mbid, score, signal_count, updated_at)
		VALUES ('u1', ?, ?, 1, ?)
	`, mbid, aff, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestSync_EmitsCandidatesForRelatedArtists(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Band", 5.0)

	today := time.Now().UTC().Format("2006-01-02")
	mb := &fakeMB{
		rels: map[string][]musicbrainz.ArtistRelation{
			"seed-1": {
				{
					Type:      "member of band",
					Direction: "backward",
					Target:    musicbrainz.Artist{MBID: "rel-1", Name: "Drummer Solo"},
				},
			},
		},
		byArtist: map[string][]musicbrainz.ReleaseGroup{
			"rel-1": {
				{MBID: "rg-new", Title: "Solo Drop", PrimaryType: "Album", FirstReleaseDate: today},
			},
		},
	}

	svc := mbrels.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok. result = %+v", r.Status, r)
	}
	if r.CandidatesNew != 1 {
		t.Fatalf("new = %d, want 1", r.CandidatesNew)
	}

	var rows int
	conn.QueryRow(`SELECT COUNT(*) FROM discover_candidates WHERE user_id='u1' AND source='mb_artist_rels'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("candidates = %d, want 1", rows)
	}
	// The related artist should have been upserted into artists so the
	// feed join lands somewhere.
	conn.QueryRow(`SELECT COUNT(*) FROM artists WHERE mbid='rel-1'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("related artist rows = %d, want 1", rows)
	}
}

func TestSync_SkipsFollowedAndHiddenTargets(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Band", 3.0)
	seedFollowed(t, conn, "already-followed", "Already Known", 1.0)
	if _, err := conn.Exec(`
		INSERT INTO hides (user_id, subject_type, subject_id, created_at)
		VALUES ('u1', 'artist', 'hidden-rel', ?)
	`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	mb := &fakeMB{
		rels: map[string][]musicbrainz.ArtistRelation{
			"seed-1": {
				{Type: "collaboration", Target: musicbrainz.Artist{MBID: "already-followed", Name: "Already Known"}},
				{Type: "collaboration", Target: musicbrainz.Artist{MBID: "hidden-rel", Name: "Hidden"}},
				{Type: "collaboration", Target: musicbrainz.Artist{MBID: "fresh-rel", Name: "Fresh"}},
			},
			"already-followed": {},
		},
		byArtist: map[string][]musicbrainz.ReleaseGroup{
			"fresh-rel": {{MBID: "rg-ok", Title: "Ok", FirstReleaseDate: today}},
		},
	}
	svc := mbrels.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.RelatedDiscovered != 1 {
		t.Fatalf("related = %d, want 1 (followed + hidden filtered)", r.RelatedDiscovered)
	}
	if r.CandidatesNew != 1 {
		t.Fatalf("new = %d, want 1", r.CandidatesNew)
	}
}

func TestSync_SkipsIfRecentRun(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Band", 5.0)
	if _, err := conn.Exec(`
		INSERT INTO sync_runs (user_id, kind, started_at, status)
		VALUES ('u1', 'mb-artist-rels', ?, 'ok')
	`, time.Now().UTC().Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	mb := &fakeMB{}
	svc := mbrels.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "skipped" {
		t.Fatalf("status = %q, want skipped", r.Status)
	}
}
