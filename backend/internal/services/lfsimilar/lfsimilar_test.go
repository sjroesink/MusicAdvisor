package lfsimilar_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/testutil"
	"github.com/sjroesink/music-advisor/backend/internal/providers/lastfm"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/lfsimilar"
)

type fakeLF struct {
	sims map[string][]lastfm.SimilarArtist
	err  error
}

func (f *fakeLF) FetchSimilarArtists(_ context.Context, seedMBID, _ string, _ int) ([]lastfm.SimilarArtist, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sims[seedMBID], nil
}

type fakeMB struct {
	byArtist map[string][]musicbrainz.ReleaseGroup
}

func (f *fakeMB) BrowseReleaseGroupsByArtist(_ context.Context, mbid string, _ int) ([]musicbrainz.ReleaseGroup, error) {
	return f.byArtist[mbid], nil
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

func TestSync_EmitsCandidatesFromLFSimilar(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Anchor", 5.0)

	today := time.Now().UTC().Format("2006-01-02")
	lf := &fakeLF{sims: map[string][]lastfm.SimilarArtist{
		"seed-1": {
			{MBID: "sim-1", Name: "Similar A", Score: 0.85},
			{MBID: "", Name: "No MBID, skipped", Score: 0.9}, // skipped
			{MBID: "sim-weak", Name: "Weak", Score: 0.1},      // below threshold
		},
	}}
	mb := &fakeMB{byArtist: map[string][]musicbrainz.ReleaseGroup{
		"sim-1": {{MBID: "rg-1", Title: "Fresh", FirstReleaseDate: today}},
	}}
	svc := lfsimilar.New(conn, lf, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok. result = %+v", r.Status, r)
	}
	if r.SimilarDiscovered != 1 {
		t.Fatalf("similar = %d, want 1 (MBIDless + weak filtered)", r.SimilarDiscovered)
	}
	if r.CandidatesNew != 1 {
		t.Fatalf("new = %d, want 1", r.CandidatesNew)
	}

	var rows int
	conn.QueryRow(`SELECT COUNT(*) FROM discover_candidates WHERE user_id='u1' AND source='lastfm_similar'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("candidates = %d, want 1", rows)
	}
}

func TestSync_SkipsFollowedSimilar(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Anchor", 3.0)
	seedFollowed(t, conn, "sim-already", "Already Followed", 1.0)

	today := time.Now().UTC().Format("2006-01-02")
	lf := &fakeLF{sims: map[string][]lastfm.SimilarArtist{
		"seed-1": {
			{MBID: "sim-already", Name: "Already Followed", Score: 0.8},
			{MBID: "sim-new", Name: "Fresh", Score: 0.7},
		},
	}}
	mb := &fakeMB{byArtist: map[string][]musicbrainz.ReleaseGroup{
		"sim-new": {{MBID: "rg-new", Title: "Ok", FirstReleaseDate: today}},
	}}
	svc := lfsimilar.New(conn, lf, mb, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.SimilarDiscovered != 1 {
		t.Fatalf("similar = %d, want 1", r.SimilarDiscovered)
	}
	if r.CandidatesNew != 1 {
		t.Fatalf("new = %d, want 1", r.CandidatesNew)
	}
}

func TestSync_SkipsIfRecentRun(t *testing.T) {
	conn := newDB(t)
	seedFollowed(t, conn, "seed-1", "Anchor", 5.0)
	if _, err := conn.Exec(`
		INSERT INTO sync_runs (user_id, kind, started_at, status)
		VALUES ('u1', 'lastfm-similar', $1, 'ok')
	`, time.Now().UTC().Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	svc := lfsimilar.New(conn, &fakeLF{}, &fakeMB{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "skipped" {
		t.Fatalf("status = %q, want skipped", r.Status)
	}
}
