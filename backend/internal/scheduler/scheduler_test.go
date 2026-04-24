package scheduler_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/db"
	"github.com/sjroesink/music-advisor/backend/internal/scheduler"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "sch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func seedConnected(t *testing.T, conn *sql.DB, userID string, needsReconnect bool) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO users(id) VALUES(?)`, userID); err != nil {
		t.Fatal(err)
	}
	needs := 0
	if needsReconnect {
		needs = 1
	}
	if _, err := conn.Exec(`
		INSERT INTO external_accounts
		  (user_id, provider, external_id, access_token_enc, refresh_token_enc,
		   needs_reconnect, connected_at)
		VALUES (?, 'spotify', ?, x'00', x'00', ?, ?)
	`, userID, userID, needs, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestScheduler_RunsJobForEveryConnectedUser(t *testing.T) {
	conn := newDB(t)
	seedConnected(t, conn, "u-alpha", false)
	seedConnected(t, conn, "u-beta", false)
	seedConnected(t, conn, "u-locked", true) // must be skipped

	var calls int32
	var seen sync.Map
	sched := scheduler.New(conn, slog.New(slog.NewTextHandler(io.Discard, nil)), scheduler.Job{
		Name:         "test",
		Interval:     50 * time.Millisecond,
		StartupDelay: 1 * time.Millisecond,
		Run: func(_ context.Context, userID string) error {
			atomic.AddInt32(&calls, 1)
			seen.Store(userID, true)
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	sched.Run(ctx)

	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("calls = %d, want ≥ 2", got)
	}
	if _, ok := seen.Load("u-locked"); ok {
		t.Fatal("needs_reconnect user must be skipped")
	}
	if _, ok := seen.Load("u-alpha"); !ok {
		t.Fatal("u-alpha never saw a run")
	}
}

func TestScheduler_PropagatesJobErrors(t *testing.T) {
	conn := newDB(t)
	seedConnected(t, conn, "u1", false)

	boom := errors.New("boom")
	var calls int32
	sched := scheduler.New(conn, slog.New(slog.NewTextHandler(io.Discard, nil)), scheduler.Job{
		Name:         "flaky",
		Interval:     30 * time.Millisecond,
		StartupDelay: 1 * time.Millisecond,
		Run: func(_ context.Context, _ string) error {
			atomic.AddInt32(&calls, 1)
			return boom
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	sched.Run(ctx)

	if atomic.LoadInt32(&calls) == 0 {
		t.Fatal("job never ran despite error")
	}
}

func TestScheduler_StopsOnContextCancel(t *testing.T) {
	conn := newDB(t)
	seedConnected(t, conn, "u1", false)

	var calls int32
	sched := scheduler.New(conn, slog.New(slog.NewTextHandler(io.Discard, nil)), scheduler.Job{
		Name:         "long",
		Interval:     20 * time.Millisecond,
		StartupDelay: 1 * time.Millisecond,
		Run: func(_ context.Context, _ string) error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("scheduler did not shut down within 200ms of cancel")
	}
	first := atomic.LoadInt32(&calls)
	time.Sleep(40 * time.Millisecond)
	if atomic.LoadInt32(&calls) != first {
		t.Fatal("scheduler kept running after context cancel")
	}
}
