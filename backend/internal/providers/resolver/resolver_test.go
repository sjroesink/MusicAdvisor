package resolver_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/db"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
)

type fakeMB struct {
	trackByISRC     func(string) (musicbrainz.Track, error)
	albumByUPC      func(string) (musicbrainz.Album, error)
	artistByName    func(string) (musicbrainz.Artist, error)
	isrcCalls       int
	upcCalls        int
	nameCalls       int
}

func (f *fakeMB) LookupTrackByISRC(_ context.Context, isrc string) (musicbrainz.Track, error) {
	f.isrcCalls++
	if f.trackByISRC == nil {
		return musicbrainz.Track{}, musicbrainz.ErrNotFound
	}
	return f.trackByISRC(isrc)
}
func (f *fakeMB) LookupAlbumByUPC(_ context.Context, upc string) (musicbrainz.Album, error) {
	f.upcCalls++
	if f.albumByUPC == nil {
		return musicbrainz.Album{}, musicbrainz.ErrNotFound
	}
	return f.albumByUPC(upc)
}
func (f *fakeMB) SearchArtistByName(_ context.Context, name string) (musicbrainz.Artist, error) {
	f.nameCalls++
	if f.artistByName == nil {
		return musicbrainz.Artist{}, musicbrainz.ErrNotFound
	}
	return f.artistByName(name)
}

func newSvc(t *testing.T, fake *fakeMB) *resolver.Service {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return resolver.New(conn, fake)
}

func TestResolveTrack_HitAndCache(t *testing.T) {
	fake := &fakeMB{
		trackByISRC: func(isrc string) (musicbrainz.Track, error) {
			return musicbrainz.Track{
				MBID: "rec-1", Title: "Spooky",
				ArtistID: "art-1", ArtistName: "Grouper",
				ReleaseGroupID: "rg-1",
			}, nil
		},
	}
	svc := newSvc(t, fake)

	// First call hits MB.
	r, err := svc.ResolveTrack(context.Background(), "sp-track-1", "USABC")
	if err != nil {
		t.Fatal(err)
	}
	if r.MBID != "rec-1" || r.ArtistMBID != "art-1" || r.ReleaseGroupID != "rg-1" {
		t.Fatalf("got %+v", r)
	}
	if fake.isrcCalls != 1 {
		t.Fatalf("isrc calls = %d after first resolve", fake.isrcCalls)
	}

	// Second call hits cache, MB is NOT touched.
	r2, err := svc.ResolveTrack(context.Background(), "sp-track-1", "USABC")
	if err != nil {
		t.Fatal(err)
	}
	if r2.MBID != "rec-1" {
		t.Fatalf("cached got %+v", r2)
	}
	if fake.isrcCalls != 1 {
		t.Fatalf("isrc calls = %d after second resolve (cache miss)", fake.isrcCalls)
	}
}

func TestResolveTrack_NotFoundIsTombstoned(t *testing.T) {
	fake := &fakeMB{} // default returns ErrNotFound
	svc := newSvc(t, fake)

	_, err := svc.ResolveTrack(context.Background(), "sp-track-x", "USZZZ")
	if !errors.Is(err, resolver.ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}

	_, err = svc.ResolveTrack(context.Background(), "sp-track-x", "USZZZ")
	if !errors.Is(err, resolver.ErrUnresolved) {
		t.Fatalf("second err = %v, want ErrUnresolved", err)
	}
	if fake.isrcCalls != 1 {
		t.Fatalf("isrc calls = %d (tombstone should suppress re-lookup)", fake.isrcCalls)
	}
}

func TestResolveTrack_EmptyISRCIsImmediateTombstone(t *testing.T) {
	fake := &fakeMB{}
	svc := newSvc(t, fake)

	_, err := svc.ResolveTrack(context.Background(), "sp-track-x", "")
	if !errors.Is(err, resolver.ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
	if fake.isrcCalls != 0 {
		t.Fatalf("isrc calls = %d; shouldn't call MB for empty ISRC", fake.isrcCalls)
	}
}

func TestResolveAlbum_OK(t *testing.T) {
	fake := &fakeMB{
		albumByUPC: func(upc string) (musicbrainz.Album, error) {
			return musicbrainz.Album{
				MBID: "rg-1", Title: "Paraphrases",
				ArtistID: "art-1", ArtistName: "Nils Frahm",
				PrimaryType: "Album", FirstReleaseDate: "2026-04-18",
			}, nil
		},
	}
	svc := newSvc(t, fake)
	r, err := svc.ResolveAlbum(context.Background(), "sp-album-1", "01234567890")
	if err != nil {
		t.Fatal(err)
	}
	if r.MBID != "rg-1" || r.PrimaryType != "Album" ||
		r.FirstReleaseDate != "2026-04-18" || r.ArtistMBID != "art-1" {
		t.Fatalf("got %+v", r)
	}
}

func TestResolveArtistByName_LowScoreTombstoned(t *testing.T) {
	fake := &fakeMB{
		artistByName: func(name string) (musicbrainz.Artist, error) {
			return musicbrainz.Artist{MBID: "a-1", Name: "ambiguous", Score: 55}, nil
		},
	}
	svc := newSvc(t, fake)
	_, err := svc.ResolveArtistByName(context.Background(), "sp-artist-1", "common name")
	if !errors.Is(err, resolver.ErrUnresolved) {
		t.Fatalf("low-score err = %v, want ErrUnresolved", err)
	}
	_, err = svc.ResolveArtistByName(context.Background(), "sp-artist-1", "common name")
	if !errors.Is(err, resolver.ErrUnresolved) {
		t.Fatalf("second err = %v, want ErrUnresolved", err)
	}
	if fake.nameCalls != 1 {
		t.Fatalf("name calls = %d, want 1 (tombstone should suppress)", fake.nameCalls)
	}
}

func TestResolveArtistByName_HighScoreOK(t *testing.T) {
	fake := &fakeMB{
		artistByName: func(name string) (musicbrainz.Artist, error) {
			return musicbrainz.Artist{MBID: "a-1", Name: "Grouper", Score: 100}, nil
		},
	}
	svc := newSvc(t, fake)
	r, err := svc.ResolveArtistByName(context.Background(), "sp-artist-1", "Grouper")
	if err != nil {
		t.Fatal(err)
	}
	if r.MBID != "a-1" || r.ArtistName != "Grouper" || r.Confidence < 0.99 {
		t.Fatalf("got %+v", r)
	}
}
