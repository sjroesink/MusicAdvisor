package library_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/testutil"
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

type recordingResolver struct {
	albums        map[string]resolver.Result
	artists       map[string]resolver.Result
	artistOrder   []string // records the order ResolveArtistByName was called in
	albumOrder    []string // records the order ResolveAlbum was called in
}

func (r *recordingResolver) ResolveAlbum(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	r.albumOrder = append(r.albumOrder, spotifyID)
	if res, ok := r.albums[spotifyID]; ok {
		return res, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

func (r *recordingResolver) ResolveArtistByName(_ context.Context, spotifyID, _ string) (resolver.Result, error) {
	r.artistOrder = append(r.artistOrder, spotifyID)
	if res, ok := r.artists[spotifyID]; ok {
		return res, nil
	}
	return resolver.Result{}, resolver.ErrUnresolved
}

// ── helpers ─────────────────────────────────────────────────────────

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn := testutil.OpenTestDB(t)
	return conn
}

func newSvc(t *testing.T, conn *sql.DB, sp *fakeSpotify, res *recordingResolver) *library.Service {
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
	svc := newSvc(t, conn, &fakeSpotify{}, &recordingResolver{})
	r, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok", r.Status)
	}
}

func TestSync_HappyPath_FollowedArtistAndAlbum(t *testing.T) {
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
	}
	res := &recordingResolver{
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
	}
	svc := newSvc(t, conn, sp, res)

	result, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("status = %q, want ok. result: %+v", result.Status, result)
	}
	if result.ArtistsAdded != 1 || result.AlbumsAdded != 1 {
		t.Fatalf("counts = %+v", result)
	}

	mustCount := func(q, name string) {
		t.Helper()
		var n int
		if err := conn.QueryRow(q).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s: n=%d err=%v", name, n, err)
		}
	}
	mustCount(`SELECT COUNT(*) FROM artists WHERE mbid='mb-ar-grouper'`, "artists MB row (Grouper)")
	mustCount(`SELECT COUNT(*) FROM artists WHERE mbid='mb-ar-nilsfrahm'`, "artists MB row (Nils Frahm)")
	mustCount(`SELECT COUNT(*) FROM albums WHERE mbid='mb-rg-paraphrases' AND type='Album'`, "album MB row")
	mustCount(`SELECT COUNT(*) FROM saved_artists WHERE user_id='u1' AND artist_mbid='mb-ar-grouper'`, "saved_artists")
	mustCount(`SELECT COUNT(*) FROM saved_albums WHERE user_id='u1' AND album_mbid='mb-rg-paraphrases'`, "saved_albums")

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
	if len(kinds) != 2 {
		t.Fatalf("signals = %v, want 2", kinds)
	}
	if kinds[0] != "follow_add" || kinds[1] != "library_add" {
		t.Fatalf("signal order = %v, want [follow_add, library_add]", kinds)
	}
}

func TestSync_UnresolvedAlbumMarksPartial(t *testing.T) {
	conn := newDB(t)
	sp := &fakeSpotify{
		albums: []spotify.SavedAlbum{
			{SpotifyID: "sp-al-x", Name: "Unknown", UPC: "xxx", ArtistID: "sp-ar-x", ArtistName: "x"},
		},
	}
	res := &recordingResolver{}
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

// TestSync_ResolveOrderFollowsRanking is the signature test for the
// prioritization goal: heavy-library artists must hit MusicBrainz first.
// Order by score DESC: followed + 3 albums > followed + 0 > 2 albums > 1.
func TestSync_ResolveOrderFollowsRanking(t *testing.T) {
	conn := newDB(t)

	mk := func(id, name, upc string) spotify.SavedAlbum {
		return spotify.SavedAlbum{
			SpotifyID: id, Name: name, AlbumType: "album",
			UPC: upc, ArtistID: id[:4] + "-ar", ArtistName: name + "-artist",
		}
	}

	sp := &fakeSpotify{
		artists: []spotify.FollowedArtist{
			// These two artists are ONLY followed (no saved albums).
			// "heavy" should be before "light" only because we prepend it
			// first in the input, which the fake preserves; actually rank
			// relies on score (both = 10). Tie-break is alphabetic by name.
			{SpotifyID: "sp-ar-alpha", Name: "Alpha"},
			{SpotifyID: "sp-ar-zeta", Name: "Zeta"},
		},
		albums: []spotify.SavedAlbum{
			// One-album artist: score = 3.
			mk("sp-al-a1", "A", "100"),
			// Two-album artist "B": score = 6.
			mk("sp-al-b1", "B", "200"),
			mk("sp-al-b2", "B", "201"),
			// Followed + three albums (artist "heavy"): score = 10 + 9 = 19.
			{SpotifyID: "sp-al-h1", Name: "H1", UPC: "300", ArtistID: "sp-ar-heavy", ArtistName: "Heavy"},
			{SpotifyID: "sp-al-h2", Name: "H2", UPC: "301", ArtistID: "sp-ar-heavy", ArtistName: "Heavy"},
			{SpotifyID: "sp-al-h3", Name: "H3", UPC: "302", ArtistID: "sp-ar-heavy", ArtistName: "Heavy"},
		},
	}
	// Also mark "heavy" as followed so its score is 10 + 9.
	sp.artists = append(sp.artists, spotify.FollowedArtist{SpotifyID: "sp-ar-heavy", Name: "Heavy"})

	res := &recordingResolver{
		artists: map[string]resolver.Result{
			"sp-ar-heavy": {MBID: "mb-heavy", ArtistMBID: "mb-heavy", ArtistName: "Heavy"},
			"sp-ar-alpha": {MBID: "mb-alpha", ArtistMBID: "mb-alpha", ArtistName: "Alpha"},
			"sp-ar-zeta":  {MBID: "mb-zeta", ArtistMBID: "mb-zeta", ArtistName: "Zeta"},
			// "B" and "A" are not in here — will be resolved via album path.
		},
		albums: map[string]resolver.Result{
			"sp-al-a1": {MBID: "mb-a1", ArtistMBID: "mb-ar-a1", ArtistName: "A-artist"},
			"sp-al-b1": {MBID: "mb-b1", ArtistMBID: "mb-ar-b",  ArtistName: "B-artist"},
			"sp-al-b2": {MBID: "mb-b2", ArtistMBID: "mb-ar-b",  ArtistName: "B-artist"},
			"sp-al-h1": {MBID: "mb-h1", ArtistMBID: "mb-heavy", ArtistName: "Heavy"},
			"sp-al-h2": {MBID: "mb-h2", ArtistMBID: "mb-heavy", ArtistName: "Heavy"},
			"sp-al-h3": {MBID: "mb-h3", ArtistMBID: "mb-heavy", ArtistName: "Heavy"},
		},
	}
	svc := newSvc(t, conn, sp, res)

	_, err := svc.Sync(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}

	// Artist-resolve order is every bucket's ResolveArtistByName call in
	// priority order. Heavy first (score 19), then Alpha + Zeta (tie at 10,
	// alpha-sort by name), then the two album-bucket artists (B score 6, A
	// score 3).
	wantArtistOrder := []string{
		"sp-ar-heavy", "sp-ar-alpha", "sp-ar-zeta",
		// "sp-al-b" bucket artist id is "sp-a" + "l-b" = "sp-al-b"... actually
		// the bucket key is the Spotify artist id, which for mk("sp-al-b1",...)
		// is the first 4 runes + "-ar" = "sp-a-ar". Bucket's SpotifyID is
		// what ResolveArtistByName receives. Let's compute:
		"sp-a-ar", // bucket for album-only artists "A" and "B" (shared prefix)
	}
	// The mk helper shares the same ArtistID for all albums ("sp-a-ar"), so
	// "A" and "B" collapse into ONE bucket with 3 albums. That bumps it to
	// score 9, still < 19. So total resolve order is:
	//   heavy (19) → alpha (10) → zeta (10) → sp-a-ar (9)
	if len(res.artistOrder) != len(wantArtistOrder) {
		t.Fatalf("artistOrder = %v, want %v", res.artistOrder, wantArtistOrder)
	}
	for i := range wantArtistOrder {
		if res.artistOrder[i] != wantArtistOrder[i] {
			t.Fatalf("artistOrder[%d] = %q, want %q (full: %v)",
				i, res.artistOrder[i], wantArtistOrder[i], res.artistOrder)
		}
	}

	// Album-resolve order: heavy's 3 albums first (because heavy's bucket is
	// first), then the merged A/B bucket's 3 albums.
	wantAlbumOrder := []string{
		"sp-al-h1", "sp-al-h2", "sp-al-h3",
		"sp-al-a1", "sp-al-b1", "sp-al-b2",
	}
	if len(res.albumOrder) != len(wantAlbumOrder) {
		t.Fatalf("albumOrder = %v, want %v", res.albumOrder, wantAlbumOrder)
	}
	for i := range wantAlbumOrder {
		if res.albumOrder[i] != wantAlbumOrder[i] {
			t.Fatalf("albumOrder[%d] = %q, want %q (full: %v)",
				i, res.albumOrder[i], wantAlbumOrder[i], res.albumOrder)
		}
	}
}
