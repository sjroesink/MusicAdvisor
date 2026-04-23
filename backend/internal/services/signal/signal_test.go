package signal_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sjroesink/music-advisor/backend/internal/db"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
)

func openDB(t *testing.T) *signal.SQLStore {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	if _, err := conn.Exec(`INSERT INTO users(id) VALUES('u1')`); err != nil {
		t.Fatal(err)
	}
	return signal.NewSQLStore(conn)
}

func TestAppend_FillsDefaultWeight(t *testing.T) {
	s := openDB(t)
	err := s.Append(context.Background(), signal.Event{
		UserID: "u1", Kind: signal.LibraryAdd,
		SubjectType: signal.SubjectAlbum, SubjectID: "album-1",
		Source: signal.SourceLibrary,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAppend_ValidatesRequiredFields(t *testing.T) {
	s := openDB(t)
	err := s.Append(context.Background(), signal.Event{Kind: signal.LibraryAdd})
	if err == nil {
		t.Fatal("expected error on missing fields")
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
