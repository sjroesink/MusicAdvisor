// Package scheduler runs per-user sync jobs on a time ticker. Every job
// owns its own interval; on each tick the scheduler walks every user with
// a connected Spotify account and calls the job's Run function. Services
// enforce their own per-user "not again this often" gate, so duplicate
// ticks are cheap skips — that's what lets us keep the scheduler logic
// here embarrassingly simple.
//
// Shutdown: Run blocks until its context is cancelled, then waits for any
// in-flight job tick to return before returning itself.
package scheduler

import (
	"context"
	"database/sql"
	"log/slog"
	"math/rand"
	"sync"
	"time"
)

// Job is one periodic task. Run is called per userID; the scheduler
// supplies a fresh, bounded context.
type Job struct {
	Name     string
	Interval time.Duration
	// StartupDelay jitters the first tick so multiple jobs don't hammer
	// the backend simultaneously at server start. Defaults to a random
	// value in [0, Interval/10) when zero.
	StartupDelay time.Duration
	// PerRunTimeout caps how long Run may take per user. Defaults to 10
	// minutes; library/top-lists can hit MB for a while on fresh accounts.
	PerRunTimeout time.Duration
	// Run is the worker. It should be idempotent (services enforce their
	// own min-interval gates).
	Run func(ctx context.Context, userID string) error
}

// Scheduler owns the lifecycle of a set of jobs against the user store.
type Scheduler struct {
	db     *sql.DB
	logger *slog.Logger
	jobs   []Job
}

func New(db *sql.DB, logger *slog.Logger, jobs ...Job) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{db: db, logger: logger, jobs: jobs}
}

// Run starts every job in its own goroutine and blocks until ctx is done.
// It returns only after every job goroutine has finished its in-flight
// tick, so callers can safely close downstream resources after Run returns.
func (s *Scheduler) Run(ctx context.Context) {
	if len(s.jobs) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, j := range s.jobs {
		wg.Add(1)
		go s.runJob(ctx, &wg, j)
	}
	wg.Wait()
}

func (s *Scheduler) runJob(ctx context.Context, wg *sync.WaitGroup, j Job) {
	defer wg.Done()

	startup := j.StartupDelay
	if startup <= 0 {
		// [0, Interval/10) keeps startup quick but avoids N jobs all
		// firing on the exact same tick.
		if j.Interval > 0 {
			//nolint:gosec // non-crypto jitter is fine here
			startup = time.Duration(rand.Int63n(int64(j.Interval / 10)))
		}
	}
	timer := time.NewTimer(startup)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	s.logger.Info("scheduler: job starting", "job", j.Name, "interval", j.Interval)
	s.tick(ctx, j)

	ticker := time.NewTicker(j.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler: job stopping", "job", j.Name)
			return
		case <-ticker.C:
			s.tick(ctx, j)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, j Job) {
	users, err := s.listUsers(ctx)
	if err != nil {
		s.logger.Warn("scheduler: list users failed", "job", j.Name, "err", err)
		return
	}
	for _, u := range users {
		if ctx.Err() != nil {
			return
		}
		timeout := j.PerRunTimeout
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		if err := j.Run(runCtx, u); err != nil {
			s.logger.Warn("scheduler: job run failed",
				"job", j.Name, "user_id", u, "err", err)
		}
		cancel()
	}
}

// listUsers returns every user_id that has a connected external account.
// Accounts marked needs_reconnect=1 are skipped since their refresh path
// will fail fast anyway — no point generating noise in the logs.
func (s *Scheduler) listUsers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT user_id FROM external_accounts
		WHERE provider = 'spotify' AND needs_reconnect = 0
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
