// Package releases discovers new releases from MusicBrainz for artists the
// user cares about, and writes the results to discover_candidates so the
// feed can surface them.
//
// Eligibility: all saved_artists for the user, plus any artist with
// artist_affinity.score ≥ MinAffinityScore (so a heavy top-rank signal
// promotes an artist the user doesn't explicitly follow). Ordered by score
// DESC; we cap at MaxArtistsPerRun to keep a single MB scan bounded at
// ~100s of seconds (MB is 1 req/s).
//
// Recency filter: only release-groups whose first_release_date falls in the
// last NewWindow days count as "new". Older releases are cataloged but not
// promoted to discover_candidates.
//
// Scoring: raw_score = recency_weight(days_old) × (1 + artist_affinity/10).
// Recency weight decays linearly from 1.0 (today) to 0.0 (NewWindow days
// ago). A top-affinity artist (score 20) dropping today gets raw_score ≈ 3;
// a seldom-heard artist (affinity 0) dropping 2 months ago gets ~0.3.
package releases

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/discover"
)

const (
	// MinInterval rate-limits the entire pass. MB is slow (1 req/s) and
	// browse responses don't change minute-to-minute.
	MinInterval = 6 * time.Hour
	// NewWindow is the cutoff for "new" releases. Spec §5.2 says 90d is a
	// sensible default; tuneable later.
	NewWindow = 90 * 24 * time.Hour
	// MaxArtistsPerRun caps MB traffic per trigger. 50 artists × 1 req/s ≈
	// 50s wall clock — acceptable for an async /api/sync/trigger phase.
	MaxArtistsPerRun = 50
	// MinAffinityScore promotes a non-followed artist into the scan if
	// signals (top_rank, library_add, heard_good) have raised affinity
	// above this floor.
	MinAffinityScore = 2.0
)

// CandidateTTL is how long a discover_candidate sticks around before
// the next pass is allowed to prune it. Resolved via the discover
// package so the policy lives in one place.
var CandidateTTL = discover.TTLForSource(discover.SourceMBReleases)

type MBClient interface {
	BrowseReleaseGroupsByArtist(ctx context.Context, artistMBID string, limit int) ([]musicbrainz.ReleaseGroup, error)
}

type Service struct {
	db     *sql.DB
	mb     MBClient
	logger *slog.Logger
	now    func() time.Time
}

func New(db *sql.DB, mb MBClient, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, mb: mb, logger: logger, now: time.Now}
}

type RunResult struct {
	RunID         int64
	Status        string // "ok" | "skipped" | "partial" | "failed"
	ArtistsScanned int
	CandidatesNew int
	CandidatesUpdated int
	Errors        int
	DurationMs    int64
	SkippedReason string
}

type artistRow struct {
	MBID   string
	Name   string
	Score  float64
}

func (s *Service) Sync(ctx context.Context, userID string) (RunResult, error) {
	started := s.now().UTC()
	result := RunResult{}

	// Rate-limit gate: look at the most recent mb-releases run.
	fresh, latest, err := s.hasFreshRun(ctx, userID, started)
	if err != nil {
		return result, err
	}
	if fresh {
		result.Status = "skipped"
		result.SkippedReason = "last mb-releases run at " + latest.Format(time.RFC3339)
		s.logger.Info("releases sync skipped (fresh run)", "user_id", userID, "latest", latest)
		return result, nil
	}

	runID, err := s.startRun(ctx, userID, "mb-releases", started)
	if err != nil {
		return result, err
	}
	result.RunID = runID

	artists, err := s.loadEligibleArtists(ctx, userID, MaxArtistsPerRun)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, err.Error(), s.now().UTC())
		result.Status = "failed"
		return result, err
	}
	s.logger.Info("releases: scanning", "user_id", userID, "artists", len(artists))

	cutoff := started.Add(-NewWindow)
	for _, a := range artists {
		result.ArtistsScanned++
		rgs, err := s.mb.BrowseReleaseGroupsByArtist(ctx, a.MBID, 25)
		if err != nil {
			s.logger.Warn("releases: browse failed", "artist_mbid", a.MBID, "err", err)
			result.Errors++
			continue
		}
		for _, rg := range rgs {
			releaseDate, ok := parseMBDate(rg.FirstReleaseDate)
			if !ok || releaseDate.Before(cutoff) {
				continue
			}
			daysOld := started.Sub(releaseDate).Hours() / 24
			score := recencyWeight(daysOld) * (1 + a.Score/10)
			reason := fmt.Sprintf(
				`{"via_artist_mbid":%q,"via_artist_name":%q,"release_date":%q,"primary_type":%q}`,
				a.MBID, a.Name, rg.FirstReleaseDate, rg.PrimaryType,
			)
			// Ensure the album row exists so the feed can look up title etc.
			if err := s.upsertAlbum(ctx, rg.MBID, a.MBID, rg.Title, rg.PrimaryType, rg.FirstReleaseDate); err != nil {
				result.Errors++
				continue
			}
			inserted, err := s.upsertCandidate(ctx, userID, rg.MBID, score, reason, started.Add(CandidateTTL))
			if err != nil {
				s.logger.Warn("releases: candidate write failed", "mbid", rg.MBID, "err", err)
				result.Errors++
				continue
			}
			if inserted {
				result.CandidatesNew++
				s.logger.Debug("new release candidate",
					"artist", a.Name, "title", rg.Title, "type", rg.PrimaryType,
					"release_date", rg.FirstReleaseDate, "score", score,
				)
			} else {
				result.CandidatesUpdated++
			}
		}
	}

	status := "ok"
	if result.Errors > 0 {
		status = "partial"
	}
	finished := s.now().UTC()
	s.finishRun(ctx, runID, status, result.CandidatesNew+result.CandidatesUpdated, "", finished)
	result.Status = status
	result.DurationMs = finished.Sub(started).Milliseconds()
	s.logger.Info("releases sync finished",
		"user_id", userID, "status", status,
		"artists", result.ArtistsScanned,
		"new", result.CandidatesNew, "updated", result.CandidatesUpdated,
		"errors", result.Errors, "duration_s", result.DurationMs/1000,
	)
	return result, nil
}

// recencyWeight decays linearly from 1.0 at day 0 to 0.0 at NewWindow days.
// Keeps fresh releases on top without dropping month-old-but-loved artists.
func recencyWeight(daysOld float64) float64 {
	if daysOld < 0 {
		daysOld = 0
	}
	windowDays := NewWindow.Hours() / 24
	w := 1 - (daysOld / windowDays)
	if w < 0 {
		return 0
	}
	return w
}

// parseMBDate handles the three MB formats: "YYYY", "YYYY-MM", "YYYY-MM-DD".
// Anything shorter (e.g. empty) is a parse failure.
func parseMBDate(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// ── DB ──────────────────────────────────────────────────────────────

func (s *Service) loadEligibleArtists(ctx context.Context, userID string, limit int) ([]artistRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.mbid, a.name, COALESCE(aa.score, 0) AS score
		FROM artists a
		LEFT JOIN artist_affinity aa
		       ON aa.artist_mbid = a.mbid AND aa.user_id = ?
		WHERE a.mbid IN (SELECT artist_mbid FROM saved_artists WHERE user_id = ?)
		   OR COALESCE(aa.score, 0) >= ?
		ORDER BY score DESC, a.name
		LIMIT ?
	`, userID, userID, MinAffinityScore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []artistRow
	for rows.Next() {
		var a artistRow
		if err := rows.Scan(&a.MBID, &a.Name, &a.Score); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) hasFreshRun(ctx context.Context, userID string, now time.Time) (bool, time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(started_at) FROM sync_runs
		WHERE user_id = ? AND kind = 'mb-releases' AND status != 'failed'
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
	return time.Time{}, errors.New("releases: unrecognized time format " + s)
}

func (s *Service) upsertAlbum(ctx context.Context, mbid, artistMBID, title, primaryType, releaseDate string) error {
	t := primaryType
	if t == "" {
		t = "Album"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO albums (mbid, primary_artist_mbid, title, release_date, type, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (mbid) DO UPDATE SET
		  primary_artist_mbid = COALESCE(excluded.primary_artist_mbid, albums.primary_artist_mbid),
		  title               = excluded.title,
		  release_date        = COALESCE(excluded.release_date, albums.release_date),
		  type                = excluded.type,
		  updated_at          = excluded.updated_at
	`, mbid, nullIfEmpty(artistMBID), title, nullIfEmpty(releaseDate), t, s.now().UTC())
	return err
}

func (s *Service) upsertCandidate(ctx context.Context, userID, albumMBID string, score float64, reason string, expiresAt time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO discover_candidates
		  (user_id, subject_type, subject_id, source, raw_score, reason_data, discovered_at, expires_at)
		VALUES (?, 'album', ?, 'mb_new_release', ?, ?, ?, ?)
		ON CONFLICT (user_id, subject_type, subject_id, source) DO UPDATE SET
		  raw_score    = excluded.raw_score,
		  reason_data  = excluded.reason_data,
		  expires_at   = excluded.expires_at
	`, userID, albumMBID, score, reason, s.now().UTC(), expiresAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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
		s.logger.Warn("releases: finishRun update failed", "err", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
