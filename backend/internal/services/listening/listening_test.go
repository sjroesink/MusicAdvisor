package listening_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/db"
	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/listening"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
)

type fakeTokens struct{}

func (fakeTokens) AccessToken(_ context.Context, _, _ string,
	_ func(ctx context.Context, ext, refresh string) (string, string, time.Time, error)) (string, error) {
	return "tok", nil
}

type fakeSpotify struct {
	recents []spotify.RecentPlay
	calls   int
}

func (f *fakeSpotify) FetchRecentlyPlayed(_ context.Context, _ string, _ time.Time) ([]spotify.RecentPlay, error) {
	f.calls++
	return f.recents, nil
}

func (f *fakeSpotify) RefreshToken(_ context.Context, _ string) (spotify.TokenSet, error) {
	return spotify.TokenSet{AccessToken: "n", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type fakeResolver struct {
	tracks map[string]resolver.Result
}

func (r *fakeResolver) ResolveTrack(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	if res, ok := r.tracks[spotifyID]; ok {
		return res, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1')`); err != nil {
		t.Fatal(err)
	}
	return conn
}

func newSvc(t *testing.T, conn *sql.DB, sp *fakeSpotify, res *fakeResolver) *listening.Service {
	t.Helper()
	return listening.New(conn, fakeTokens{}, sp, res,
		signal.NewSQLStore(conn),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func signalKindsFor(t *testing.T, conn *sql.DB, mbid string) []string {
	t.Helper()
	rows, err := conn.Query(`
		SELECT kind FROM signals WHERE user_id='u1' AND subject_id=? AND subject_type='track' ORDER BY id
	`, mbid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		rows.Scan(&k)
		out = append(out, k)
	}
	return out
}

func TestSync_FirstPoll_EmitsFullVsSkipByPair(t *testing.T) {
	conn := newDB(t)
	base := time.Now().UTC().Truncate(time.Second).Add(-1 * time.Hour)

	// Track A (180s): played at base. Played in full → next play starts at
	// base+180s exactly, threshold = base+180s-30s = base+150s < next(180s)
	// → full. Track B (240s): played at base+180s. Skipped — next at
	// base+200s (only 20s in). Track C: played at base+200s, newest in this
	// poll, no signal emitted yet (no successor).
	plays := []spotify.RecentPlay{
		{SpotifyID: "sp-a", Name: "A", DurationMs: 180000, ISRC: "US-A", PlayedAt: base},
		{SpotifyID: "sp-b", Name: "B", DurationMs: 240000, ISRC: "US-B", PlayedAt: base.Add(180 * time.Second)},
		{SpotifyID: "sp-c", Name: "C", DurationMs: 200000, ISRC: "US-C", PlayedAt: base.Add(200 * time.Second)},
	}
	sp := &fakeSpotify{recents: plays}
	res := &fakeResolver{tracks: map[string]resolver.Result{
		"sp-a": {MBID: "mb-a", ArtistMBID: "mb-ar", ArtistName: "Ar"},
		"sp-b": {MBID: "mb-b", ArtistMBID: "mb-ar", ArtistName: "Ar"},
		"sp-c": {MBID: "mb-c", ArtistMBID: "mb-ar", ArtistName: "Ar"},
	}}
	svc := newSvc(t, conn, sp, res)

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok. result: %+v", r.Status, r)
	}
	if r.Inserted != 3 {
		t.Fatalf("inserted = %d, want 3", r.Inserted)
	}
	if r.PlayedFull != 1 || r.Skipped != 1 {
		t.Fatalf("full=%d skipped=%d, want 1 & 1. result: %+v", r.PlayedFull, r.Skipped, r)
	}
	if kinds := signalKindsFor(t, conn, "mb-a"); len(kinds) != 1 || kinds[0] != "play_full" {
		t.Fatalf("mb-a signals = %v, want [play_full]", kinds)
	}
	if kinds := signalKindsFor(t, conn, "mb-b"); len(kinds) != 1 || kinds[0] != "play_skip" {
		t.Fatalf("mb-b signals = %v, want [play_skip]", kinds)
	}
	if kinds := signalKindsFor(t, conn, "mb-c"); len(kinds) != 0 {
		t.Fatalf("mb-c signals = %v, want none (last in poll)", kinds)
	}

	var rows int
	conn.QueryRow(`SELECT COUNT(*) FROM play_history WHERE user_id='u1'`).Scan(&rows)
	if rows != 3 {
		t.Fatalf("play_history rows = %d, want 3", rows)
	}
}

func TestSync_SecondPoll_EmitsForPreviousPollLastPlay(t *testing.T) {
	conn := newDB(t)
	base := time.Now().UTC().Truncate(time.Second).Add(-1 * time.Hour)

	// First poll: only track A.
	sp := &fakeSpotify{recents: []spotify.RecentPlay{
		{SpotifyID: "sp-a", Name: "A", DurationMs: 180000, ISRC: "US-A", PlayedAt: base},
	}}
	res := &fakeResolver{tracks: map[string]resolver.Result{
		"sp-a": {MBID: "mb-a", ArtistMBID: "mb-ar", ArtistName: "Ar"},
		"sp-b": {MBID: "mb-b", ArtistMBID: "mb-ar", ArtistName: "Ar"},
	}}
	svc := newSvc(t, conn, sp, res)
	if _, err := svc.Sync(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if kinds := signalKindsFor(t, conn, "mb-a"); len(kinds) != 0 {
		t.Fatalf("after poll 1 mb-a signals = %v, want none", kinds)
	}

	// Second poll: track B arrives. A's fate becomes known via the pair
	// (mb-a, mb-b). A's duration is 180s, B starts at base+100s → threshold
	// = base+150s > base+100s → A was skipped.
	sp.recents = []spotify.RecentPlay{
		{SpotifyID: "sp-b", Name: "B", DurationMs: 240000, ISRC: "US-B", PlayedAt: base.Add(100 * time.Second)},
	}
	if _, err := svc.Sync(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if kinds := signalKindsFor(t, conn, "mb-a"); len(kinds) != 1 || kinds[0] != "play_skip" {
		t.Fatalf("after poll 2 mb-a signals = %v, want [play_skip]", kinds)
	}
	if kinds := signalKindsFor(t, conn, "mb-b"); len(kinds) != 0 {
		t.Fatalf("after poll 2 mb-b signals = %v, want none", kinds)
	}
}

func TestSync_ReRun_EmitsNoExtraSignals(t *testing.T) {
	conn := newDB(t)
	base := time.Now().UTC().Truncate(time.Second).Add(-1 * time.Hour)
	sp := &fakeSpotify{recents: []spotify.RecentPlay{
		{SpotifyID: "sp-a", Name: "A", DurationMs: 180000, ISRC: "US-A", PlayedAt: base},
		{SpotifyID: "sp-b", Name: "B", DurationMs: 240000, ISRC: "US-B", PlayedAt: base.Add(180 * time.Second)},
	}}
	res := &fakeResolver{tracks: map[string]resolver.Result{
		"sp-a": {MBID: "mb-a", ArtistMBID: "mb-ar", ArtistName: "Ar"},
		"sp-b": {MBID: "mb-b", ArtistMBID: "mb-ar", ArtistName: "Ar"},
	}}
	svc := newSvc(t, conn, sp, res)
	if _, err := svc.Sync(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	// Second call returns same Spotify payload — no new plays, no new signals.
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Inserted != 0 || r.PlayedFull != 0 || r.Skipped != 0 {
		t.Fatalf("re-run result: %+v, want zero inserted/full/skipped", r)
	}
	// mb-a should have exactly one play_full signal.
	if kinds := signalKindsFor(t, conn, "mb-a"); len(kinds) != 1 {
		t.Fatalf("mb-a signals = %v, want exactly 1 after re-run", kinds)
	}
}

func TestSync_UnresolvedTrack_StillRecordsHistory(t *testing.T) {
	conn := newDB(t)
	base := time.Now().UTC().Truncate(time.Second)
	sp := &fakeSpotify{recents: []spotify.RecentPlay{
		{SpotifyID: "sp-x", Name: "X", DurationMs: 180000, ISRC: "", PlayedAt: base},
	}}
	res := &fakeResolver{} // empty — will ErrUnresolved
	svc := newSvc(t, conn, sp, res)

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1", r.Unresolved)
	}
	var n int
	conn.QueryRow(`SELECT COUNT(*) FROM play_history WHERE user_id='u1' AND spotify_track_id='sp-x'`).Scan(&n)
	if n != 1 {
		t.Fatalf("play_history = %d, want 1 (even when unresolved)", n)
	}
}
