// Package toplists pulls Spotify top artists for the three listening
// ranges (short/medium/long_term), resolves them to MBIDs, writes a row to
// top_snapshots per (user, time_range, rank), and emits top_rank signals.
//
// Top tracks and recently-played are deferred: the user asked to keep the
// library sync bounded to "albums + artists". Track signals live in a later
// sub-phase alongside play_full / play_skip.
//
// Guard rail: the Sync refuses to run if the user already has a top_snapshot
// within MinInterval. This stops repeated /api/sync/trigger calls from
// inflating top_rank affinity by multiple full days in a morning. Phase 8
// adds a real scheduler that enforces 24h cadence; until then this guard
// doubles as both a rate limit and a deduper.
package toplists

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
)

// MinInterval is the shortest gap between two top-lists syncs for one user.
// 12h ≈ "twice a day max"; aligned with the spec's 24h tick but looser so
// operators can force a refresh after 12h without waiting a full day.
const MinInterval = 12 * time.Hour

// TokenProvider is the same shape library.TokenProvider uses. Kept local so
// a future extract can drop the package without a cross-service import.
type TokenProvider interface {
	AccessToken(ctx context.Context, userID, provider string,
		refresh func(ctx context.Context, externalID, refreshToken string) (string, string, time.Time, error),
	) (string, error)
}

// SpotifyClient is the slice of *spotify.Client we depend on.
type SpotifyClient interface {
	FetchTopArtists(ctx context.Context, accessToken string, tr spotify.TopTimeRange) ([]spotify.TopArtist, error)
	RefreshToken(ctx context.Context, refreshToken string) (spotify.TokenSet, error)
}

// Resolver is the minimal surface needed to map a top artist to an MBID.
type Resolver interface {
	ResolveArtistByName(ctx context.Context, spotifyID, name string) (resolver.Result, error)
}

type Service struct {
	db       *sql.DB
	tokens   TokenProvider
	spotify  SpotifyClient
	resolver Resolver
	signals  signal.Writer
	logger   *slog.Logger
	now      func() time.Time
}

func New(
	db *sql.DB,
	tokens TokenProvider,
	sp SpotifyClient,
	res Resolver,
	sigs signal.Writer,
	logger *slog.Logger,
) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db: db, tokens: tokens, spotify: sp, resolver: res,
		signals: sigs, logger: logger, now: time.Now,
	}
}

// RunResult summarizes one top-lists sync.
type RunResult struct {
	RunID        int64
	Status       string // "ok" | "skipped" | "partial" | "failed"
	Ranges       int    // how many ranges were actually fetched this run
	ArtistsSeen  int    // total top artists iterated across ranges
	Resolved     int    // had an MBID and were snapshotted+signalled
	Unresolved   int
	Errors       int
	DurationMs   int64
	SkippedReason string // populated when Status == "skipped"
}

// rangeWeightMult maps each time_range to its top_rank weight multiplier.
// Spec 4.4: short=1.5 (most recent listening), medium=1.0, long=0.7.
var rangeWeightMult = map[spotify.TopTimeRange]float64{
	spotify.TopRangeShort:  1.5,
	spotify.TopRangeMedium: 1.0,
	spotify.TopRangeLong:   0.7,
}

// Sync pulls top artists for all three ranges and writes snapshots + signals.
// Returns RunResult{Status: "skipped"} if the last snapshot is < MinInterval
// old — callers should surface this distinctly from a real failure.
func (s *Service) Sync(ctx context.Context, userID string) (RunResult, error) {
	started := s.now().UTC()
	result := RunResult{}

	// Freshness gate: if any snapshot exists within MinInterval, skip.
	fresh, latest, err := s.hasFreshSnapshot(ctx, userID, started)
	if err != nil {
		return result, err
	}
	if fresh {
		result.Status = "skipped"
		result.SkippedReason = "last snapshot at " + latest.Format(time.RFC3339)
		s.logger.Info("toplists skipped (fresh snapshot)", "user_id", userID, "latest", latest)
		return result, nil
	}

	runID, err := s.startRun(ctx, userID, "spotify-top", started)
	if err != nil {
		return result, err
	}
	result.RunID = runID

	accessToken, err := s.tokens.AccessToken(ctx, userID, "spotify",
		func(ctx context.Context, _, refresh string) (string, string, time.Time, error) {
			ts, err := s.spotify.RefreshToken(ctx, refresh)
			if err != nil {
				return "", "", time.Time{}, err
			}
			return ts.AccessToken, ts.RefreshToken, ts.ExpiresAt, nil
		})
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, err.Error(), s.now().UTC())
		result.Status = "failed"
		return result, err
	}

	snapshotAt := s.now().UTC()
	for _, tr := range []spotify.TopTimeRange{
		spotify.TopRangeShort, spotify.TopRangeMedium, spotify.TopRangeLong,
	} {
		s.logger.Info("fetching top artists", "user_id", userID, "range", tr)
		artists, err := s.spotify.FetchTopArtists(ctx, accessToken, tr)
		if err != nil {
			s.logger.Warn("fetch top artists failed", "user_id", userID, "range", tr, "err", err)
			result.Errors++
			continue
		}
		result.Ranges++
		result.ArtistsSeen += len(artists)

		mult := rangeWeightMult[tr]
		for _, a := range artists {
			s.resolveOne(ctx, userID, a, tr, mult, snapshotAt, &result)
		}
	}

	status := "ok"
	if result.Errors > 0 || result.Unresolved > 0 {
		status = "partial"
	}
	if result.Ranges == 0 {
		status = "failed"
	}

	finished := s.now().UTC()
	s.finishRun(ctx, runID, status, result.Resolved, "", finished)
	result.Status = status
	result.DurationMs = finished.Sub(started).Milliseconds()
	s.logger.Info("toplists sync finished",
		"user_id", userID, "status", status,
		"ranges", result.Ranges, "artists_seen", result.ArtistsSeen,
		"resolved", result.Resolved, "unresolved", result.Unresolved,
		"errors", result.Errors, "duration_s", result.DurationMs/1000,
	)
	return result, nil
}

func (s *Service) resolveOne(ctx context.Context, userID string, a spotify.TopArtist,
	tr spotify.TopTimeRange, mult float64, snapshotAt time.Time, r *RunResult) {

	res, err := s.resolver.ResolveArtistByName(ctx, a.SpotifyID, a.Name)
	if errors.Is(err, resolver.ErrUnresolved) {
		r.Unresolved++
		return
	}
	if err != nil {
		s.logger.Warn("resolve top artist failed", "spotify_id", a.SpotifyID, "err", err)
		r.Errors++
		return
	}

	if err := s.upsertArtist(ctx, res.MBID, a.SpotifyID, a.Name); err != nil {
		r.Errors++
		return
	}
	if err := s.insertSnapshot(ctx, userID, string(tr), a.Rank, res.MBID, snapshotAt); err != nil {
		r.Errors++
		return
	}
	weight := topRankWeight(a.Rank, mult)
	if err := s.signals.Append(ctx, signal.Event{
		UserID:      userID,
		Kind:        signal.TopRank,
		SubjectType: signal.SubjectArtist,
		SubjectID:   res.MBID,
		Source:      signal.SourceTop,
		Weight:      weight,
		Context:     fmt.Sprintf(`{"range":%q,"rank":%d}`, string(tr), a.Rank),
		Timestamp:   snapshotAt,
	}); err != nil {
		r.Errors++
		return
	}
	r.Resolved++
	s.logger.Debug("top artist scored",
		"range", tr, "rank", a.Rank, "mbid", res.MBID, "name", a.Name, "weight", weight,
	)
}

// topRankWeight implements spec §4.4: 2.0 × (1 - rank/50) × range_mult.
// Rank is 1-based; rank 1 gives (1 - 0.02) * mult, rank 50 gives 0.
func topRankWeight(rank int, mult float64) float64 {
	if rank < 1 {
		rank = 1
	}
	return 2.0 * (1 - float64(rank)/50.0) * mult
}

// ── DB ──────────────────────────────────────────────────────────────

// hasFreshSnapshot reads MAX(snapshot_at) as string because modernc's sqlite
// driver doesn't round-trip DATETIME columns into time.Time cleanly. Values
// written by us always land in one of time.RFC3339 or SQLite's "YYYY-MM-DD
// HH:MM:SS" form, so we try both.
func (s *Service) hasFreshSnapshot(ctx context.Context, userID string, now time.Time) (bool, time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(snapshot_at) FROM top_snapshots
		WHERE user_id = ? AND kind = 'artist'
	`, userID).Scan(&raw)
	if err != nil {
		return false, time.Time{}, err
	}
	if !raw.Valid || raw.String == "" {
		return false, time.Time{}, nil
	}
	latest, err := parseSQLiteTime(raw.String)
	if err != nil {
		return false, time.Time{}, err
	}
	return now.Sub(latest) < MinInterval, latest, nil
}

func parseSQLiteTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("top_snapshots: unrecognized time format %q", s)
}

func (s *Service) upsertArtist(ctx context.Context, mbid, spotifyID, name string) error {
	if mbid == "" {
		return errors.New("upsertArtist: mbid required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artists (mbid, spotify_id, name, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (mbid) DO UPDATE SET
		  spotify_id = COALESCE(excluded.spotify_id, artists.spotify_id),
		  name       = excluded.name,
		  updated_at = excluded.updated_at
	`, mbid, nullIfEmpty(spotifyID), name, s.now().UTC())
	return err
}

func (s *Service) insertSnapshot(ctx context.Context, userID, timeRange string, rank int, mbid string, snapshotAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO top_snapshots (user_id, kind, time_range, rank, subject_mbid, snapshot_at)
		VALUES (?, 'artist', ?, ?, ?, ?)
	`, userID, timeRange, rank, mbid, snapshotAt)
	return err
}

func (s *Service) startRun(ctx context.Context, userID, kind string, started time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_runs (user_id, kind, started_at, status)
		VALUES (?, ?, ?, 'running')
	`, userID, kind, started)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Service) finishRun(ctx context.Context, id int64, status string, itemsAdded int, errText string, finished time.Time) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sync_runs
		SET status = ?, finished_at = ?, items_added = ?, error = NULLIF(?, '')
		WHERE id = ?
	`, status, finished, itemsAdded, errText, id)
	if err != nil {
		s.logger.Warn("toplists: finishRun update failed", "err", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
