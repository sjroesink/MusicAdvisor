package toplists_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/testutil"
	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
	"github.com/sjroesink/music-advisor/backend/internal/services/toplists"
)

// ── fakes ───────────────────────────────────────────────────────────

type fakeTokens struct{ token string }

func (f *fakeTokens) AccessToken(_ context.Context, _, _ string,
	_ func(ctx context.Context, ext, refresh string) (string, string, time.Time, error)) (string, error) {
	return f.token, nil
}

type fakeSpotify struct {
	byRange       map[spotify.TopTimeRange][]spotify.TopArtist
	tracksByRange map[spotify.TopTimeRange][]spotify.TopTrack
	errors        map[spotify.TopTimeRange]error
	calls         int
}

func (f *fakeSpotify) FetchTopArtists(_ context.Context, _ string, tr spotify.TopTimeRange) ([]spotify.TopArtist, error) {
	f.calls++
	if err, ok := f.errors[tr]; ok {
		return nil, err
	}
	return f.byRange[tr], nil
}

func (f *fakeSpotify) FetchTopTracks(_ context.Context, _ string, tr spotify.TopTimeRange) ([]spotify.TopTrack, error) {
	f.calls++
	return f.tracksByRange[tr], nil
}

func (f *fakeSpotify) RefreshToken(_ context.Context, _ string) (spotify.TokenSet, error) {
	return spotify.TokenSet{AccessToken: "n", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type fakeResolver struct {
	artists map[string]resolver.Result
	tracks  map[string]resolver.Result
	albums  map[string]resolver.Result
}

func (r *fakeResolver) ResolveArtistByName(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	if res, ok := r.artists[spotifyID]; ok {
		return res, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

func (r *fakeResolver) ResolveTrack(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	if res, ok := r.tracks[spotifyID]; ok {
		return res, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

func (r *fakeResolver) ResolveAlbum(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	if res, ok := r.albums[spotifyID]; ok {
		return res, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

// ── helpers ─────────────────────────────────────────────────────────

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn := testutil.OpenTestDB(t)
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1')`); err != nil {
		t.Fatal(err)
	}
	return conn
}

func newSvc(t *testing.T, conn *sql.DB, sp *fakeSpotify, res *fakeResolver) *toplists.Service {
	t.Helper()
	return toplists.New(conn, &fakeTokens{token: "tok"}, sp, res,
		signal.NewSQLStore(conn),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// ── tests ───────────────────────────────────────────────────────────

func TestSync_HappyPath_AllThreeRanges(t *testing.T) {
	conn := newDB(t)
	sp := &fakeSpotify{byRange: map[spotify.TopTimeRange][]spotify.TopArtist{
		spotify.TopRangeShort: {
			{SpotifyID: "sp-1", Name: "First", Rank: 1},
			{SpotifyID: "sp-2", Name: "Second", Rank: 2},
		},
		spotify.TopRangeMedium: {{SpotifyID: "sp-1", Name: "First", Rank: 1}},
		spotify.TopRangeLong:   {{SpotifyID: "sp-2", Name: "Second", Rank: 1}},
	}}
	res := &fakeResolver{artists: map[string]resolver.Result{
		"sp-1": {MBID: "mb-1", ArtistMBID: "mb-1", ArtistName: "First"},
		"sp-2": {MBID: "mb-2", ArtistMBID: "mb-2", ArtistName: "Second"},
	}}
	svc := newSvc(t, conn, sp, res)

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok. result: %+v", r.Status, r)
	}
	if r.Ranges != 3 || r.ArtistsDone != 4 {
		t.Fatalf("counts = %+v", r)
	}

	// Snapshots: short(2) + medium(1) + long(1) = 4 rows
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM top_snapshots WHERE user_id='u1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("snapshot rows = %d, want 4", n)
	}

	// Artists upserted
	if err := conn.QueryRow(`SELECT COUNT(*) FROM artists WHERE mbid IN ('mb-1','mb-2')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("artists rows = %d, want 2", n)
	}

	// Signals: one top_rank per snapshot
	if err := conn.QueryRow(`SELECT COUNT(*) FROM signals WHERE user_id='u1' AND kind='top_rank'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("top_rank signals = %d, want 4", n)
	}

	// Weight sanity: rank 1 short_term = 2.0 * (1 - 1/50) * 1.5 = 2.94
	var weight float64
	if err := conn.QueryRow(`
		SELECT weight FROM signals WHERE user_id='u1' AND kind='top_rank'
		  AND subject_id='mb-1' AND context LIKE '%short_term%' LIMIT 1
	`).Scan(&weight); err != nil {
		t.Fatal(err)
	}
	want := 2.0 * (1 - 1.0/50.0) * 1.5
	if math.Abs(weight-want) > 1e-9 {
		t.Fatalf("weight = %v, want %v", weight, want)
	}
}

func TestSync_FreshSnapshotIsSkipped(t *testing.T) {
	conn := newDB(t)
	// Seed a recent snapshot — within MinInterval (12h).
	if _, err := conn.Exec(`
		INSERT INTO top_snapshots (user_id, kind, time_range, rank, subject_mbid, snapshot_at)
		VALUES ('u1','artist','short_term',1,'mb-prior',$1)
	`, time.Now().UTC().Add(-1*time.Hour)); err != nil {
		t.Fatal(err)
	}
	sp := &fakeSpotify{}
	res := &fakeResolver{}
	svc := newSvc(t, conn, sp, res)

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "skipped" {
		t.Fatalf("status = %q, want skipped. result: %+v", r.Status, r)
	}
	if sp.calls != 0 {
		t.Fatalf("sp.calls = %d, want 0 (no Spotify hits after skip)", sp.calls)
	}
}

func TestSync_UnresolvedArtistsCountButDontBlock(t *testing.T) {
	conn := newDB(t)
	sp := &fakeSpotify{byRange: map[spotify.TopTimeRange][]spotify.TopArtist{
		spotify.TopRangeShort: {
			{SpotifyID: "sp-known", Name: "Known", Rank: 1},
			{SpotifyID: "sp-unknown", Name: "Unknown", Rank: 2},
		},
		spotify.TopRangeMedium: {},
		spotify.TopRangeLong:   {},
	}}
	res := &fakeResolver{artists: map[string]resolver.Result{
		"sp-known": {MBID: "mb-k", ArtistMBID: "mb-k", ArtistName: "Known"},
	}}
	svc := newSvc(t, conn, sp, res)

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "partial" {
		t.Fatalf("status = %q, want partial", r.Status)
	}
	if r.ArtistsDone != 1 || r.Unresolved != 1 {
		t.Fatalf("counts = %+v", r)
	}
}

func TestSync_TopTracks_CreatesCatalogAndSignals(t *testing.T) {
	conn := newDB(t)
	sp := &fakeSpotify{
		byRange: map[spotify.TopTimeRange][]spotify.TopArtist{
			spotify.TopRangeShort: {}, spotify.TopRangeMedium: {}, spotify.TopRangeLong: {},
		},
		tracksByRange: map[spotify.TopTimeRange][]spotify.TopTrack{
			spotify.TopRangeShort: {
				{SpotifyID: "sp-t1", Name: "Track One", DurationMs: 240000, ISRC: "US-A",
					AlbumID: "sp-al1", AlbumName: "Album", ArtistID: "sp-ar1", ArtistName: "Artist", Rank: 1},
			},
			spotify.TopRangeMedium: {}, spotify.TopRangeLong: {},
		},
	}
	res := &fakeResolver{
		tracks: map[string]resolver.Result{
			"sp-t1": {MBID: "mb-t1", ArtistMBID: "mb-ar1", ArtistName: "Artist",
				ReleaseGroupID: "mb-rg1", Title: "Track One"},
		},
	}
	svc := newSvc(t, conn, sp, res)

	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.TracksDone != 1 {
		t.Fatalf("tracks_done = %d, want 1. result: %+v", r.TracksDone, r)
	}

	var n int
	conn.QueryRow(`SELECT COUNT(*) FROM tracks WHERE mbid='mb-t1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("tracks row = %d, want 1", n)
	}
	conn.QueryRow(`SELECT COUNT(*) FROM albums WHERE mbid='mb-rg1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("albums row = %d, want 1", n)
	}
	conn.QueryRow(`SELECT COUNT(*) FROM signals WHERE subject_type='track' AND subject_id='mb-t1' AND kind='top_rank'`).Scan(&n)
	if n != 1 {
		t.Fatalf("top_rank track signal = %d, want 1", n)
	}
	conn.QueryRow(`SELECT COUNT(*) FROM top_snapshots WHERE kind='track' AND subject_mbid='mb-t1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("top_snapshots track = %d, want 1", n)
	}
}

func TestSync_RangeWeightMultipliers(t *testing.T) {
	conn := newDB(t)
	sp := &fakeSpotify{byRange: map[spotify.TopTimeRange][]spotify.TopArtist{
		spotify.TopRangeShort:  {{SpotifyID: "sp-1", Name: "A", Rank: 1}},
		spotify.TopRangeMedium: {{SpotifyID: "sp-1", Name: "A", Rank: 1}},
		spotify.TopRangeLong:   {{SpotifyID: "sp-1", Name: "A", Rank: 1}},
	}}
	res := &fakeResolver{artists: map[string]resolver.Result{
		"sp-1": {MBID: "mb-1", ArtistMBID: "mb-1", ArtistName: "A"},
	}}
	svc := newSvc(t, conn, sp, res)

	if _, err := svc.Sync(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}

	check := func(rangeName string, wantMult float64) {
		t.Helper()
		var w float64
		q := `SELECT weight FROM signals WHERE user_id='u1' AND kind='top_rank'
		      AND subject_id='mb-1' AND context LIKE '%' || $1 || '%' LIMIT 1`
		if err := conn.QueryRow(q, rangeName).Scan(&w); err != nil {
			t.Fatal(err)
		}
		want := 2.0 * (1 - 1.0/50.0) * wantMult
		if math.Abs(w-want) > 1e-9 {
			t.Fatalf("%s weight = %v, want %v", rangeName, w, want)
		}
	}
	check("short_term", 1.5)
	check("medium_term", 1.0)
	check("long_term", 0.7)
}
