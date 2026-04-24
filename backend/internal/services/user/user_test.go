package user_test

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/db"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
)

func newSvc(t *testing.T) *user.Service {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	key := make([]byte, 32)
	rand.Read(key)
	cipher, err := auth.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return user.NewService(conn, cipher)
}

func TestUpsertByExternal_CreatesThenUpdates(t *testing.T) {
	svc := newSvc(t)

	id1, err := svc.UpsertByExternal(context.Background(), user.ExternalAccount{
		Provider:       "spotify",
		ExternalID:     "sander",
		AccessToken:    "acc-1",
		RefreshToken:   "ref-1",
		TokenExpiresAt: time.Now().Add(time.Hour),
		Scopes:         "user-library-read",
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if id1 == "" {
		t.Fatal("empty user id")
	}

	id2, err := svc.UpsertByExternal(context.Background(), user.ExternalAccount{
		Provider:       "spotify",
		ExternalID:     "sander",
		AccessToken:    "acc-2",
		RefreshToken:   "ref-2",
		TokenExpiresAt: time.Now().Add(time.Hour),
		Scopes:         "user-library-read user-top-read",
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("expected same user id on repeat upsert, got %q vs %q", id2, id1)
	}
}

func TestAccessToken_ReturnsNonExpiredWithoutRefresh(t *testing.T) {
	svc := newSvc(t)
	id, err := svc.UpsertByExternal(context.Background(), user.ExternalAccount{
		Provider:       "spotify",
		ExternalID:     "sander",
		AccessToken:    "acc-live",
		RefreshToken:   "ref",
		TokenExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	refreshCalled := false
	got, err := svc.AccessToken(context.Background(), id, "spotify",
		func(ctx context.Context, ext, refresh string) (string, string, time.Time, error) {
			refreshCalled = true
			return "", "", time.Time{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if refreshCalled {
		t.Fatal("refresh should not run when token is fresh")
	}
	if got != "acc-live" {
		t.Fatalf("got %q, want acc-live", got)
	}
}

func TestAccessToken_RefreshesWhenExpired(t *testing.T) {
	svc := newSvc(t)
	id, err := svc.UpsertByExternal(context.Background(), user.ExternalAccount{
		Provider:       "spotify",
		ExternalID:     "sander",
		AccessToken:    "acc-stale",
		RefreshToken:   "ref-current",
		TokenExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := svc.AccessToken(context.Background(), id, "spotify",
		func(ctx context.Context, ext, refresh string) (string, string, time.Time, error) {
			if ext != "sander" {
				t.Errorf("external_id = %q", ext)
			}
			if refresh != "ref-current" {
				t.Errorf("refresh_token = %q", refresh)
			}
			return "acc-new", "ref-new", time.Now().Add(time.Hour), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got != "acc-new" {
		t.Fatalf("got %q, want acc-new", got)
	}

	// Next call should serve the freshly stored access token without a refresh.
	second, err := svc.AccessToken(context.Background(), id, "spotify",
		func(context.Context, string, string) (string, string, time.Time, error) {
			t.Fatal("refresh should not run twice in a row")
			return "", "", time.Time{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if second != "acc-new" {
		t.Fatalf("second call got %q, want acc-new", second)
	}
}

func TestAccessToken_TransientRefreshFailureDoesNotLock(t *testing.T) {
	svc := newSvc(t)
	id, err := svc.UpsertByExternal(context.Background(), user.ExternalAccount{
		Provider:       "spotify",
		ExternalID:     "sander",
		AccessToken:    "acc-stale",
		RefreshToken:   "ref",
		TokenExpiresAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	transient := errors.New("upstream 500")
	_, err = svc.AccessToken(context.Background(), id, "spotify",
		func(context.Context, string, string) (string, string, time.Time, error) {
			return "", "", time.Time{}, transient
		})
	if !errors.Is(err, transient) {
		t.Fatalf("err = %v, want transient", err)
	}

	// A second call tries to refresh again — account must NOT be locked.
	called := false
	_, err = svc.AccessToken(context.Background(), id, "spotify",
		func(context.Context, string, string) (string, string, time.Time, error) {
			called = true
			return "", "", time.Time{}, transient
		})
	if !called {
		t.Fatal("refresh should retry after a transient failure")
	}
	if errors.Is(err, user.ErrNeedsReconnect) {
		t.Fatal("transient failure must not lock the account")
	}
}

func TestAccessToken_TerminalRefreshLocksAccount(t *testing.T) {
	svc := newSvc(t)
	id, err := svc.UpsertByExternal(context.Background(), user.ExternalAccount{
		Provider:       "spotify",
		ExternalID:     "sander",
		AccessToken:    "acc-stale",
		RefreshToken:   "ref",
		TokenExpiresAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	terminal := errors.New("invalid_grant")
	_, err = svc.AccessToken(context.Background(), id, "spotify",
		func(context.Context, string, string) (string, string, time.Time, error) {
			return "", "", time.Time{}, user.AsTerminal(terminal)
		})
	if !errors.Is(err, terminal) {
		t.Fatalf("err = %v, want the wrapped terminal cause", err)
	}
	if !errors.Is(err, user.ErrTerminalRefresh) {
		t.Fatalf("err = %v, want ErrTerminalRefresh sentinel", err)
	}

	// Next call must refuse to retry and return ErrNeedsReconnect.
	_, err = svc.AccessToken(context.Background(), id, "spotify",
		func(context.Context, string, string) (string, string, time.Time, error) {
			t.Fatal("refresh should not run once the account is locked")
			return "", "", time.Time{}, nil
		})
	if !errors.Is(err, user.ErrNeedsReconnect) {
		t.Fatalf("err = %v, want ErrNeedsReconnect", err)
	}
}
