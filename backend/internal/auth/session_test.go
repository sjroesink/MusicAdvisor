package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/testutil"
)

func TestSessionStore_CreateGetDelete(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	defer conn.Close()

	userID := "u-1"
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}

	store := auth.NewSessionStore(conn)
	ctx := context.Background()

	sess, err := store.Create(ctx, userID, "test-ua")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("empty session id")
	}

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != userID {
		t.Fatalf("UserID = %q, want %q", got.UserID, userID)
	}
	if !got.ExpiresAt.After(time.Now()) {
		t.Fatalf("ExpiresAt should be in future")
	}

	if err := store.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, sess.ID); !errors.Is(err, auth.ErrNoSession) {
		t.Fatalf("after delete: err=%v, want ErrNoSession", err)
	}
}

func TestSessionStore_Get_ExpiredReturnsExpired(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	defer conn.Close()

	userID := "u-1"
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES ($1)`, userID); err != nil {
		t.Fatal(err)
	}

	store := auth.NewSessionStore(conn)
	sess, err := store.Create(context.Background(), userID, "")
	if err != nil {
		t.Fatal(err)
	}

	// Manually expire.
	if _, err := conn.Exec(
		`UPDATE sessions SET expires_at = $1 WHERE id = $2`,
		time.Now().Add(-time.Hour), sess.ID,
	); err != nil {
		t.Fatal(err)
	}

	_, err = store.Get(context.Background(), sess.ID)
	if !errors.Is(err, auth.ErrSessionExpired) {
		t.Fatalf("err = %v, want ErrSessionExpired", err)
	}
}
