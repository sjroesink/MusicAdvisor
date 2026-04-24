package samelabel_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/testutil"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/samelabel"
)

type fakeMB struct {
	artistRGs    map[string][]musicbrainz.ReleaseGroup
	rgLabels     map[string][]musicbrainz.Label
	labelRGs     map[string][]musicbrainz.ReleaseGroup
	browseErr    error
	labelErr     error
}

func (f *fakeMB) BrowseReleaseGroupsByArtist(_ context.Context, mbid string, _ int) ([]musicbrainz.ReleaseGroup, error) {
	if f.browseErr != nil {
		return nil, f.browseErr
	}
	return f.artistRGs[mbid], nil
}
func (f *fakeMB) ReleaseGroupLabels(_ context.Context, mbid string) ([]musicbrainz.Label, error) {
	if f.labelErr != nil {
		return nil, f.labelErr
	}
	return f.rgLabels[mbid], nil
}
func (f *fakeMB) BrowseReleaseGroupsByLabel(_ context.Context, mbid string, _ int) ([]musicbrainz.ReleaseGroup, error) {
	return f.labelRGs[mbid], nil
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn := testutil.OpenTestDB(t)
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1')`); err != nil {
		t.Fatal(err)
	}
	return conn
}

func seedFollowed(t *testing.T, conn *sql.DB, mbid, name string, aff float64) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO artists (mbid, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, mbid, name); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		INSERT INTO saved_artists (user_id, artist_mbid, saved_at)
		VALUES ('u1', $1, $2) ON CONFLICT DO NOTHING
	`, mbid, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		INSERT INTO artist_affinity (user_id, artist_mbid, score, signal_count, updated_at)
		VALUES ('u1', $1, $2, 1, $3)
	`, mbid, aff, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestSync_EmitsCandidatesFromSharedLabel(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Known", 5.0)

	today := time.Now().UTC().Format("2006-01-02")
	mb := &fakeMB{
		artistRGs: map[string][]musicbrainz.ReleaseGroup{
			"seed-1": {{MBID: "seed-rg", Title: "Anchor", FirstReleaseDate: today}},
		},
		rgLabels: map[string][]musicbrainz.Label{
			"seed-rg": {{MBID: "lbl-xl", Name: "XL Recordings"}},
		},
		labelRGs: map[string][]musicbrainz.ReleaseGroup{
			"lbl-xl": {
				{MBID: "rg-label-a", Title: "Labelmate A", FirstReleaseDate: today, ArtistID: "ar-a", ArtistName: "Labelmate A"},
				{MBID: "rg-label-b", Title: "Labelmate B", FirstReleaseDate: today, ArtistID: "ar-b", ArtistName: "Labelmate B"},
			},
		},
	}
	svc := samelabel.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok. result = %+v", r.Status, r)
	}
	if r.LabelsDiscovered != 1 {
		t.Fatalf("labels = %d, want 1", r.LabelsDiscovered)
	}
	if r.CandidatesNew != 2 {
		t.Fatalf("new = %d, want 2", r.CandidatesNew)
	}

	var rows int
	conn.QueryRow(`SELECT COUNT(*) FROM discover_candidates WHERE user_id='u1' AND source='mb_same_label'`).Scan(&rows)
	if rows != 2 {
		t.Fatalf("candidates = %d, want 2", rows)
	}
	conn.QueryRow(`SELECT COUNT(*) FROM labels WHERE mbid='lbl-xl'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("label rows = %d, want 1", rows)
	}
}

func TestSync_SkipsAlreadySavedAlbums(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Known", 2.0)
	// Pretend the user already has rg-label-a saved; it should not become a candidate.
	if _, err := conn.Exec(`
		INSERT INTO albums (mbid, title, type) VALUES ('rg-label-a', 'Already', 'Album')
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		INSERT INTO saved_albums (user_id, album_mbid, saved_at)
		VALUES ('u1', 'rg-label-a', $1)
	`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	mb := &fakeMB{
		artistRGs: map[string][]musicbrainz.ReleaseGroup{
			"seed-1": {{MBID: "seed-rg", Title: "Anchor", FirstReleaseDate: today}},
		},
		rgLabels: map[string][]musicbrainz.Label{
			"seed-rg": {{MBID: "lbl-xl", Name: "XL"}},
		},
		labelRGs: map[string][]musicbrainz.ReleaseGroup{
			"lbl-xl": {
				{MBID: "rg-label-a", Title: "Already", FirstReleaseDate: today}, // skipped (saved)
				{MBID: "rg-label-b", Title: "Fresh",   FirstReleaseDate: today}, // kept
			},
		},
	}
	svc := samelabel.New(conn, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.CandidatesNew != 1 {
		t.Fatalf("new = %d, want 1", r.CandidatesNew)
	}
	var who string
	conn.QueryRow(`SELECT subject_id FROM discover_candidates WHERE user_id='u1' AND source='mb_same_label'`).Scan(&who)
	if who != "rg-label-b" {
		t.Fatalf("candidate = %q, want rg-label-b", who)
	}
}

func TestSync_SkipsIfRecentRun(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Known", 5.0)
	if _, err := conn.Exec(`
		INSERT INTO sync_runs (user_id, kind, started_at, status)
		VALUES ('u1', 'mb-same-label', $1, 'ok')
	`, time.Now().UTC().Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	svc := samelabel.New(conn, &fakeMB{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "skipped" {
		t.Fatalf("status = %q, want skipped", r.Status)
	}
}
