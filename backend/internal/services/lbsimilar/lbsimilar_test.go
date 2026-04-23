package lbsimilar_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/db"
	"github.com/sjroesink/music-advisor/backend/internal/providers/listenbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/lbsimilar"
)

type fakeLB struct {
	byArtist map[string][]listenbrainz.SimilarArtist
	calls    int
}

func (f *fakeLB) FetchSimilarArtists(_ context.Context, mbid string, _ int) ([]listenbrainz.SimilarArtist, error) {
	f.calls++
	return f.byArtist[mbid], nil
}

type fakeMB struct {
	byArtist map[string][]musicbrainz.ReleaseGroup
	calls    int
}

func (f *fakeMB) BrowseReleaseGroupsByArtist(_ context.Context, mbid string, _ int) ([]musicbrainz.ReleaseGroup, error) {
	f.calls++
	return f.byArtist[mbid], nil
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1')`); err != nil {
		t.Fatal(err)
	}
	return conn
}

func seedArtist(t *testing.T, conn *sql.DB, mbid, name string, affinity float64, followed bool) {
	t.Helper()
	conn.Exec(`INSERT INTO artists (mbid, name) VALUES (?, ?) ON CONFLICT DO NOTHING`, mbid, name)
	if followed {
		conn.Exec(`
			INSERT INTO saved_artists (user_id, artist_mbid, saved_at) VALUES ('u1', ?, ?) ON CONFLICT DO NOTHING
		`, mbid, time.Now().UTC())
	}
	if affinity != 0 {
		conn.Exec(`
			INSERT INTO artist_affinity (user_id, artist_mbid, score, signal_count, updated_at)
			VALUES ('u1', ?, ?, 1, ?)
			ON CONFLICT (user_id, artist_mbid) DO UPDATE SET score = excluded.score
		`, mbid, affinity, time.Now().UTC())
	}
}

func TestSync_ProducesCandidatesFromSimilarArtists(t *testing.T) {
	conn := newDB(t)
	seedArtist(t, conn, "mb-seed", "Seed", 5.0, true)

	today := time.Now().UTC().Format("2006-01-02")
	lb := &fakeLB{byArtist: map[string][]listenbrainz.SimilarArtist{
		"mb-seed": {
			{MBID: "mb-sim-1", Name: "Sim 1", Score: 0.85},
			{MBID: "mb-seed", Name: "Seed", Score: 1.0}, // should be skipped by LB client; we test svc resilience anyway
		},
	}}
	mb := &fakeMB{byArtist: map[string][]musicbrainz.ReleaseGroup{
		"mb-sim-1": {
			{MBID: "mb-rg-1", Title: "Fresh", PrimaryType: "Album", FirstReleaseDate: today},
			{MBID: "mb-rg-2", Title: "Older", PrimaryType: "Album", FirstReleaseDate: "2020-01-01"},
		},
	}}
	svc := lbsimilar.New(conn, lb, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok. result: %+v", r.Status, r)
	}
	if r.SeedsScanned != 1 || r.SimilarDiscovered != 1 {
		t.Fatalf("counts = %+v", r)
	}
	// Default ReleasesPerSimilar = 2 → both rgs become candidates (both the
	// 2026-today one and the 2020-01-01 one; no recency filter in this
	// service, that's the releases service's job).
	if r.CandidatesNew != 2 {
		t.Fatalf("new candidates = %d, want 2", r.CandidatesNew)
	}
}

func TestSync_SkipsFollowedAndHiddenSimilarArtists(t *testing.T) {
	conn := newDB(t)
	seedArtist(t, conn, "mb-seed", "Seed", 5.0, true)
	// A "similar" artist that's already saved.
	seedArtist(t, conn, "mb-sim-saved", "Saved Already", 0, true)
	// A hidden similar artist.
	conn.Exec(`
		INSERT INTO hides (user_id, subject_type, subject_id, created_at)
		VALUES ('u1','artist','mb-sim-hidden',?)
	`, time.Now().UTC())

	lb := &fakeLB{byArtist: map[string][]listenbrainz.SimilarArtist{
		"mb-seed": {
			{MBID: "mb-sim-saved", Name: "Saved", Score: 0.9},
			{MBID: "mb-sim-hidden", Name: "Hidden", Score: 0.9},
			{MBID: "mb-sim-new", Name: "New", Score: 0.5},
		},
	}}
	mb := &fakeMB{byArtist: map[string][]musicbrainz.ReleaseGroup{
		"mb-sim-new": {{MBID: "mb-rg-new", Title: "N", FirstReleaseDate: "2026-04-01"}},
	}}
	svc := lbsimilar.New(conn, lb, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.SimilarDiscovered != 1 {
		t.Fatalf("similar = %d, want 1 (only mb-sim-new). result: %+v", r.SimilarDiscovered, r)
	}
	if r.CandidatesNew != 1 {
		t.Fatalf("candidates = %d, want 1", r.CandidatesNew)
	}
}

func TestSync_FreshRunIsSkipped(t *testing.T) {
	conn := newDB(t)
	seedArtist(t, conn, "mb-seed", "Seed", 5.0, true)
	conn.Exec(`
		INSERT INTO sync_runs (user_id, kind, started_at, status)
		VALUES ('u1','lb-similar',?,'ok')
	`, time.Now().UTC().Add(-10*time.Minute))

	lb := &fakeLB{}
	mb := &fakeMB{}
	svc := lbsimilar.New(conn, lb, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "skipped" {
		t.Fatalf("status = %q, want skipped", r.Status)
	}
	if lb.calls != 0 || mb.calls != 0 {
		t.Fatalf("calls = lb=%d mb=%d, want 0 each", lb.calls, mb.calls)
	}
}
