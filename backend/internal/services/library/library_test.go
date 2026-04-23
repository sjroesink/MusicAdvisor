package library_test

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
	"github.com/sjroesink/music-advisor/backend/internal/services/library"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
)

// ── fakes ───────────────────────────────────────────────────────────

type fakeTokens struct{ token string }

func (f *fakeTokens) AccessToken(ctx context.Context, _, _ string,
	_ func(ctx context.Context, ext, refresh string) (string, string, time.Time, error)) (string, error) {
	return f.token, nil
}

type fakeSpotify struct {
	albums  []spotify.SavedAlbum
	tracks  []spotify.SavedTrack
	artists []spotify.FollowedArtist
}

func (f *fakeSpotify) FetchSavedAlbums(_ context.Context, _ string, onAlbum func(spotify.SavedAlbum) error) (int, error) {
	for _, a := range f.albums {
		if err := onAlbum(a); err != nil {
			return 0, err
		}
	}
	return len(f.albums), nil
}

func (f *fakeSpotify) FetchSavedTracks(_ context.Context, _ string, onTrack func(spotify.SavedTrack) error) (int, error) {
	for _, t := range f.tracks {
		if err := onTrack(t); err != nil {
			return 0, err
		}
	}
	return len(f.tracks), nil
}

func (f *fakeSpotify) FetchFollowedArtists(_ context.Context, _ string, onArtist func(spotify.FollowedArtist) error) (int, error) {
	for _, a := range f.artists {
		if err := onArtist(a); err != nil {
			return 0, err
		}
	}
	return len(f.artists), nil
}

func (f *fakeSpotify) RefreshToken(_ context.Context, _ string) (spotify.TokenSet, error) {
	return spotify.TokenSet{AccessToken: "new", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type fakeResolver struct {
	albums  map[string]resolver.Result
	tracks  map[string]resolver.Result
	artists map[string]resolver.Result
}

func (f *fakeResolver) ResolveAlbum(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	if r, ok := f.albums[spotifyID]; ok {
		return r, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

func (f *fakeResolver) ResolveTrack(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	if r, ok := f.tracks[spotifyID]; ok {
		return r, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

func (f *fakeResolver) ResolveArtistByName(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	if r, ok := f.artists[spotifyID]; ok {
		return r, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

// ── helpers ─────────────────────────────────────────────────────────

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "lib.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func newSvc(t *testing.T, conn *sql.DB, sp *fakeSpotify, res *fakeResolver) *library.Service {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	return library.New(conn, &fakeTokens{token: "tok"}, sp, res,
		signal.NewSQLStore(conn),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// ── tests ───────────────────────────────────────────────────────────

func TestSync_NothingToDo_ReturnsOK(t *testing.T) {
	conn := newDB(t)
	svc := newSvc(t, conn, &fakeSpotify{}, &fakeResolver{})
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok", r.Status)
	}
}

func TestSync_FullLibraryHappyPath(t *testing.T) {
	conn := newDB(t)
	sp := &fakeSpotify{
		artists: []spotify.FollowedArtist{
			{SpotifyID: "sp-ar-1", Name: "Grouper"},
		},
		albums: []spotify.SavedAlbum{
			{
				SpotifyID: "sp-al-1", Name: "Paraphrases",
				AlbumType: "album", ReleaseDate: "2026-04-18",
				TotalTracks: 11, UPC: "0123",
				ArtistID: "sp-ar-2", ArtistName: "Nils Frahm",
				AddedAt: time.Now(),
			},
		},
		tracks: []spotify.SavedTrack{
			{
				SpotifyID: "sp-tr-1", Name: "Spooky",
				DurationMs: 240000, ISRC: "USABC",
				AlbumID: "sp-al-2", AlbumName: "Ruins",
				ArtistID: "sp-ar-1", ArtistName: "Grouper",
				AddedAt: time.Now(),
			},
		},
	}
	res := &fakeResolver{
		artists: map[string]resolver.Result{
			"sp-ar-1": {MBID: "mb-ar-grouper", Confidence: 1.0, ArtistMBID: "mb-ar-grouper", ArtistName: "Grouper"},
		},
		albums: map[string]resolver.Result{
			"sp-al-1": {
				MBID: "mb-rg-paraphrases", Confidence: 0.9,
				ArtistMBID: "mb-ar-nilsfrahm", ArtistName: "Nils Frahm",
				Title: "Paraphrases", PrimaryType: "Album", FirstReleaseDate: "2026-04-18",
			},
		},
		tracks: map[string]resolver.Result{
			"sp-tr-1": {
				MBID: "mb-rec-spooky", Confidence: 0.95,
				ArtistMBID: "mb-ar-grouper", ArtistName: "Grouper",
				ReleaseGroupID: "mb-rg-ruins", Title: "Spooky",
			},
		},
	}
	svc := newSvc(t, conn, sp, res)

	result, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("status = %q, want ok. result: %+v", result.Status, result)
	}
	if result.ArtistsAdded != 1 || result.AlbumsAdded != 1 || result.TracksAdded != 1 {
		t.Fatalf("counts = %+v", result)
	}

	var n int
	mustCount := func(q, name string) {
		t.Helper()
		if err := conn.QueryRow(q).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: n=%d err=%v", name, n, err)
		}
	}
	mustCount(`SELECT COUNT(*) FROM artists WHERE mbid='mb-ar-grouper'`, "artists MB row")
	mustCount(`SELECT COUNT(*) FROM albums WHERE mbid='mb-rg-paraphrases' AND type='Album'`, "album MB row")
	mustCount(`SELECT COUNT(*) FROM tracks WHERE mbid='mb-rec-spooky'`, "track MB row")
	mustCount(`SELECT COUNT(*) FROM saved_artists WHERE user_id='u1' AND artist_mbid='mb-ar-grouper'`, "saved_artists")
	mustCount(`SELECT COUNT(*) FROM saved_albums WHERE user_id='u1' AND album_mbid='mb-rg-paraphrases'`, "saved_albums")
	mustCount(`SELECT COUNT(*) FROM saved_tracks WHERE user_id='u1' AND track_mbid='mb-rec-spooky'`, "saved_tracks")

	rows, err := conn.Query(`SELECT kind FROM signals WHERE user_id='u1' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	if len(kinds) != 3 {
		t.Fatalf("signals = %v, want 3", kinds)
	}
	if kinds[0] != "follow_add" || kinds[1] != "library_add" || kinds[2] != "library_add" {
		t.Fatalf("signal order = %v", kinds)
	}

	var (
		status string
		items  int
	)
	if err := conn.QueryRow(`SELECT status, items_added FROM sync_runs WHERE user_id='u1'`).Scan(&status, &items); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || items != 3 {
		t.Fatalf("sync_runs status=%q items=%d", status, items)
	}
}

func TestSync_UnresolvedItemsMarkPartial(t *testing.T) {
	conn := newDB(t)
	sp := &fakeSpotify{
		albums: []spotify.SavedAlbum{
			{SpotifyID: "sp-al-x", Name: "Unknown", UPC: "xxx", ArtistID: "sp-ar-x", ArtistName: "x"},
		},
	}
	res := &fakeResolver{}
	svc := newSvc(t, conn, sp, res)

	result, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "partial" {
		t.Fatalf("status = %q, want partial", result.Status)
	}
	if result.Unresolved != 1 || result.AlbumsAdded != 0 {
		t.Fatalf("counts = %+v", result)
	}
}

func TestSync_TrackWithoutMBAlbumUsesPlaceholder(t *testing.T) {
	conn := newDB(t)
	sp := &fakeSpotify{
		tracks: []spotify.SavedTrack{
			{
				SpotifyID: "sp-tr-1", Name: "Spooky", DurationMs: 240000, ISRC: "USABC",
				AlbumID: "sp-al-orphan", AlbumName: "Unknown Album",
				ArtistID: "sp-ar-1", ArtistName: "Grouper",
				AddedAt: time.Now(),
			},
		},
	}
	res := &fakeResolver{
		tracks: map[string]resolver.Result{
			"sp-tr-1": {
				MBID: "mb-rec-spooky", Confidence: 0.95,
				ArtistMBID: "mb-ar-grouper", ArtistName: "Grouper",
				// ReleaseGroupID intentionally empty — MB hit via ISRC has no
				// release-group for this track, so the service must fall back
				// to an sp:-prefixed placeholder so the track FK holds.
				Title: "Spooky",
			},
		},
	}
	svc := newSvc(t, conn, sp, res)

	result, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.TracksAdded != 1 {
		t.Fatalf("result = %+v", result)
	}

	var placeholderCount int
	if err := conn.QueryRow(
		`SELECT COUNT(*) FROM albums WHERE mbid='sp:sp-al-orphan' AND spotify_id='sp-al-orphan'`,
	).Scan(&placeholderCount); err != nil {
		t.Fatal(err)
	}
	if placeholderCount != 1 {
		t.Fatalf("expected placeholder album row; got %d", placeholderCount)
	}
}
