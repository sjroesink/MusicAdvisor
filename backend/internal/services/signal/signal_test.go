package signal_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/testutil"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
)

func openDB(t *testing.T) (*signal.SQLStore, *sql.DB) {
	t.Helper()
	conn := testutil.OpenTestDB(t)

	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1')`); err != nil {
		t.Fatal(err)
	}
	return signal.NewSQLStore(conn), conn
}

func seedArtistAlbum(t *testing.T, conn *sql.DB, artistMBID, albumMBID string) {
	t.Helper()
	if _, err := conn.Exec(`
		INSERT INTO artists (mbid, name) VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, artistMBID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`
		INSERT INTO albums (mbid, primary_artist_mbid, title, type)
		VALUES ($1, $2, $3, 'Album')
		ON CONFLICT DO NOTHING
	`, albumMBID, artistMBID, "t"); err != nil {
		t.Fatal(err)
	}
}

func score(t *testing.T, conn *sql.DB, table, key, id string) (float64, int) {
	t.Helper()
	var score float64
	var count int
	q := `SELECT score, signal_count FROM ` + table + ` WHERE user_id='u1' AND ` + key + `=$1`
	if err := conn.QueryRow(q, id).Scan(&score, &count); err != nil {
		if err == sql.ErrNoRows {
			return 0, 0
		}
		t.Fatal(err)
	}
	return score, count
}

func TestAppend_FillsDefaultWeight(t *testing.T) {
	s, conn := openDB(t)
	if _, err := conn.Exec(`INSERT INTO artists (mbid, name) VALUES ('ar-1','a')`); err != nil {
		t.Fatal(err)
	}
	err := s.Append(context.Background(), signal.Event{
		UserID: "u1", Kind: signal.FollowAdd,
		SubjectType: signal.SubjectArtist, SubjectID: "ar-1",
		Source: signal.SourceLibrary,
	})
	if err != nil {
		t.Fatal(err)
	}
	sc, n := score(t, conn, "artist_affinity", "artist_mbid", "ar-1")
	if sc != 1.2 || n != 1 {
		t.Fatalf("score=%v count=%d, want 1.2/1", sc, n)
	}
}

func TestAppend_ValidatesRequiredFields(t *testing.T) {
	s, _ := openDB(t)
	err := s.Append(context.Background(), signal.Event{Kind: signal.LibraryAdd})
	if err == nil {
		t.Fatal("expected error on missing fields")
	}
}

func TestAppend_AlbumPropagatesToArtist(t *testing.T) {
	s, conn := openDB(t)
	seedArtistAlbum(t, conn, "ar-1", "al-1")

	err := s.Append(context.Background(), signal.Event{
		UserID: "u1", Kind: signal.LibraryAdd,
		SubjectType: signal.SubjectAlbum, SubjectID: "al-1",
		Source: signal.SourceLibrary,
	})
	if err != nil {
		t.Fatal(err)
	}

	// album gets full 1.0
	sc, _ := score(t, conn, "album_affinity", "album_mbid", "al-1")
	if sc != 1.0 {
		t.Fatalf("album score = %v, want 1.0", sc)
	}
	// artist gets 1.0 * 0.5 = 0.5 (from propagation only)
	sc, _ = score(t, conn, "artist_affinity", "artist_mbid", "ar-1")
	if sc != 0.5 {
		t.Fatalf("artist score = %v, want 0.5 (propagated)", sc)
	}
}

func TestAppend_RepeatedSignals_Accumulate(t *testing.T) {
	s, conn := openDB(t)
	if _, err := conn.Exec(`INSERT INTO artists (mbid, name) VALUES ('ar-1','a')`); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := s.Append(context.Background(), signal.Event{
			UserID: "u1", Kind: signal.HeardGood,
			SubjectType: signal.SubjectArtist, SubjectID: "ar-1",
			Source: signal.SourceUI,
		}); err != nil {
			t.Fatal(err)
		}
	}
	sc, n := score(t, conn, "artist_affinity", "artist_mbid", "ar-1")
	if sc != 4.5 || n != 3 {
		t.Fatalf("score=%v count=%d, want 4.5/3", sc, n)
	}
	var rating string
	if err := conn.QueryRow(`SELECT rating FROM ratings WHERE user_id='u1' AND subject_id='ar-1'`).Scan(&rating); err != nil {
		t.Fatal(err)
	}
	if rating != "good" {
		t.Fatalf("rating = %q, want good", rating)
	}
}

func TestAppend_DismissWritesHide(t *testing.T) {
	s, conn := openDB(t)
	seedArtistAlbum(t, conn, "ar-1", "al-1")

	if err := s.Append(context.Background(), signal.Event{
		UserID: "u1", Kind: signal.Dismiss,
		SubjectType: signal.SubjectAlbum, SubjectID: "al-1",
		Source: signal.SourceUI,
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM hides WHERE user_id='u1' AND subject_type='album' AND subject_id='al-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("hides count = %d, want 1", n)
	}
}

func TestAppend_HeardBadOverwritesHeardGood(t *testing.T) {
	s, conn := openDB(t)
	if _, err := conn.Exec(`INSERT INTO artists (mbid, name) VALUES ('ar-1','a')`); err != nil {
		t.Fatal(err)
	}

	if err := s.Append(context.Background(), signal.Event{
		UserID: "u1", Kind: signal.HeardGood,
		SubjectType: signal.SubjectArtist, SubjectID: "ar-1",
		Source: signal.SourceUI,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(context.Background(), signal.Event{
		UserID: "u1", Kind: signal.HeardBad,
		SubjectType: signal.SubjectArtist, SubjectID: "ar-1",
		Source: signal.SourceUI,
	}); err != nil {
		t.Fatal(err)
	}
	var rating string
	if err := conn.QueryRow(`SELECT rating FROM ratings WHERE user_id='u1' AND subject_id='ar-1'`).Scan(&rating); err != nil {
		t.Fatal(err)
	}
	if rating != "bad" {
		t.Fatalf("rating = %q, want bad (later signal wins)", rating)
	}
}

// TestRebuild_MirrorsAppendBehavior — Rebuild on raw signals must land the
// same affinity numbers as replaying them through Append.
func TestRebuild_MirrorsAppendBehavior(t *testing.T) {
	s, conn := openDB(t)
	seedArtistAlbum(t, conn, "ar-1", "al-1")

	events := []signal.Event{
		{Kind: signal.FollowAdd, SubjectType: signal.SubjectArtist, SubjectID: "ar-1"},
		{Kind: signal.LibraryAdd, SubjectType: signal.SubjectAlbum, SubjectID: "al-1"},
		{Kind: signal.HeardGood, SubjectType: signal.SubjectAlbum, SubjectID: "al-1"},
	}
	for _, e := range events {
		e.UserID = "u1"
		e.Source = signal.SourceLibrary
		if err := s.Append(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	wantArtistScore, wantArtistCount := score(t, conn, "artist_affinity", "artist_mbid", "ar-1")
	wantAlbumScore, wantAlbumCount := score(t, conn, "album_affinity", "album_mbid", "al-1")

	if err := s.Rebuild(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	gotArtistScore, gotArtistCount := score(t, conn, "artist_affinity", "artist_mbid", "ar-1")
	gotAlbumScore, gotAlbumCount := score(t, conn, "album_affinity", "album_mbid", "al-1")

	if gotArtistScore != wantArtistScore || gotArtistCount != wantArtistCount {
		t.Fatalf("artist: got (%v,%d), want (%v,%d)",
			gotArtistScore, gotArtistCount, wantArtistScore, wantArtistCount)
	}
	if gotAlbumScore != wantAlbumScore || gotAlbumCount != wantAlbumCount {
		t.Fatalf("album: got (%v,%d), want (%v,%d)",
			gotAlbumScore, gotAlbumCount, wantAlbumScore, wantAlbumCount)
	}
}

func TestDefaultWeight_Spec(t *testing.T) {
	cases := map[signal.Kind]float64{
		signal.LibraryAdd: 1.0,
		signal.FollowAdd:  1.2,
		signal.HeardGood:  1.5,
		signal.HeardBad:   -1.5,
		signal.Dismiss:    -0.5,
		signal.PlaySkip:   -0.3,
	}
	for k, want := range cases {
		if got := signal.DefaultWeight(k); got != want {
			t.Errorf("DefaultWeight(%q) = %v, want %v", k, got, want)
		}
	}
}
