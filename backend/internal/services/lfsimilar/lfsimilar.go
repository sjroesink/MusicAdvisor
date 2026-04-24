// Package lfsimilar writes Last.fm-sourced discover candidates.
//
// Mirrors lbsimilar: for each top-affinity seed artist, ask Last.fm for
// similar artists, filter by saved/hidden, then grab recent release-groups
// from MB for each accepted similar. Where Last.fm returns an MBID we use
// it directly; for MBID-less hits we skip (resolving by name alone is too
// ambiguous to be trusted).
//
// Scoring: raw_score = lf_match × (1 + seed_affinity/10).
package lfsimilar

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/lastfm"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/discover"
)

const (
	// MinInterval matches lb-similar: 1h is enough so repeated triggers
	// don't keep hammering Last.fm, but short enough that a feed refresh
	// feels live.
	MinInterval = 1 * time.Hour
	// MaxSeedArtists caps how many seeds we branch off of.
	MaxSeedArtists = 20
	// MaxSimilarPerSeed caps how many Last.fm results we act on per seed.
	MaxSimilarPerSeed = 5
	// ReleasesPerSimilar is how many recent release-groups we surface per
	// discovered similar artist.
	ReleasesPerSimilar = 2
	// MinMatchScore filters weak Last.fm matches.
	MinMatchScore = 0.3
)

// CandidateTTL is resolved via the shared discover policy (spec §5.4 / §11).
var CandidateTTL = discover.TTLForSource(discover.SourceLastfmSim)

type LFClient interface {
	FetchSimilarArtists(ctx context.Context, seedMBID, seedName string, limit int) ([]lastfm.SimilarArtist, error)
}

type MBClient interface {
	BrowseReleaseGroupsByArtist(ctx context.Context, artistMBID string, limit int) ([]musicbrainz.ReleaseGroup, error)
}

type Service struct {
	db     *sql.DB
	lf     LFClient
	mb     MBClient
	logger *slog.Logger
	now    func() time.Time
}

func New(db *sql.DB, lf LFClient, mb MBClient, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, lf: lf, mb: mb, logger: logger, now: time.Now}
}

type RunResult struct {
	RunID             int64
	Status            string
	SeedsScanned      int
	SimilarDiscovered int
	CandidatesNew     int
	CandidatesUpdated int
	Errors            int
	DurationMs        int64
	SkippedReason     string
}

type seedArtist struct {
	MBID  string
	Name  string
	Score float64
}

func (s *Service) Sync(ctx context.Context, userID string) (RunResult, error) {
	started := s.now().UTC()
	result := RunResult{}

	fresh, latest, err := s.hasFreshRun(ctx, userID, started)
	if err != nil {
		return result, err
	}
	if fresh {
		result.Status = "skipped"
		result.SkippedReason = "last lastfm-similar run at " + latest.Format(time.RFC3339)
		s.logger.Info("lfsimilar skipped (fresh run)", "user_id", userID, "latest", latest)
		return result, nil
	}

	runID, err := s.startRun(ctx, userID, "lastfm-similar", started)
	if err != nil {
		return result, err
	}
	result.RunID = runID

	seeds, err := s.loadSeeds(ctx, userID, MaxSeedArtists)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, err.Error(), s.now().UTC())
		result.Status = "failed"
		return result, err
	}
	if len(seeds) == 0 {
		s.finishRun(ctx, runID, "ok", 0, "", s.now().UTC())
		result.Status = "ok"
		return result, nil
	}

	excluded, err := s.loadExclusions(ctx, userID)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, err.Error(), s.now().UTC())
		result.Status = "failed"
		return result, err
	}

	expires := started.Add(CandidateTTL)
	for _, seed := range seeds {
		result.SeedsScanned++
		sims, err := s.lf.FetchSimilarArtists(ctx, seed.MBID, seed.Name, MaxSimilarPerSeed*3)
		if err != nil {
			s.logger.Warn("lfsimilar: LF fetch failed", "seed", seed.MBID, "err", err)
			result.Errors++
			continue
		}

		picked := 0
		for _, sim := range sims {
			if picked >= MaxSimilarPerSeed {
				break
			}
			// Last.fm hits without an MBID can't join to MB's release-group
			// browse, which drives everything downstream. Skip rather than
			// risk a name-collision surfacing the wrong discography.
			if sim.MBID == "" {
				continue
			}
			if sim.Score < MinMatchScore {
				continue
			}
			if excluded[sim.MBID] {
				continue
			}
			result.SimilarDiscovered++

			rgs, err := s.mb.BrowseReleaseGroupsByArtist(ctx, sim.MBID, 10)
			if err != nil {
				s.logger.Warn("lfsimilar: MB browse failed", "similar", sim.MBID, "err", err)
				result.Errors++
				continue
			}
			sort.SliceStable(rgs, func(i, j int) bool {
				return rgs[i].FirstReleaseDate > rgs[j].FirstReleaseDate
			})
			if len(rgs) > ReleasesPerSimilar {
				rgs = rgs[:ReleasesPerSimilar]
			}

			if sim.Name != "" {
				_ = s.upsertArtist(ctx, sim.MBID, sim.Name)
			}

			for _, rg := range rgs {
				score := sim.Score * (1 + seed.Score/10)
				reason := fmt.Sprintf(
					`{"via_artist_mbid":%q,"via_artist_name":%q,"lf_match":%f,"seed_affinity":%f,"release_date":%q}`,
					sim.MBID, sim.Name, sim.Score, seed.Score, rg.FirstReleaseDate,
				)
				if err := s.upsertAlbum(ctx, rg.MBID, sim.MBID, rg.Title, rg.PrimaryType, rg.FirstReleaseDate); err != nil {
					result.Errors++
					continue
				}
				inserted, err := s.upsertCandidate(ctx, userID, rg.MBID, score, reason, expires)
				if err != nil {
					s.logger.Warn("lfsimilar: candidate write failed", "mbid", rg.MBID, "err", err)
					result.Errors++
					continue
				}
				if inserted {
					result.CandidatesNew++
				} else {
					result.CandidatesUpdated++
				}
			}
			picked++
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
	s.logger.Info("lfsimilar sync finished",
		"user_id", userID, "status", status,
		"seeds", result.SeedsScanned,
		"similar", result.SimilarDiscovered,
		"new", result.CandidatesNew, "updated", result.CandidatesUpdated,
		"errors", result.Errors, "duration_s", result.DurationMs/1000,
	)
	return result, nil
}

// ── DB helpers ──

func (s *Service) loadSeeds(ctx context.Context, userID string, limit int) ([]seedArtist, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.mbid, a.name, COALESCE(aa.score, 0) AS score
		FROM artists a
		LEFT JOIN artist_affinity aa
		       ON aa.artist_mbid = a.mbid AND aa.user_id = $1
		WHERE a.mbid IN (SELECT artist_mbid FROM saved_artists WHERE user_id = $2)
		   OR COALESCE(aa.score, 0) > 0
		ORDER BY score DESC, a.name
		LIMIT $3
	`, userID, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []seedArtist
	for rows.Next() {
		var a seedArtist
		if err := rows.Scan(&a.MBID, &a.Name, &a.Score); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Service) loadExclusions(ctx context.Context, userID string) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT artist_mbid FROM saved_artists WHERE user_id = $1`, userID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return nil, err
		}
		out[m] = true
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `
		SELECT subject_id FROM hides WHERE user_id = $1 AND subject_type = 'artist'
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out[m] = true
	}
	return out, rows.Err()
}

func (s *Service) hasFreshRun(ctx context.Context, userID string, now time.Time) (bool, time.Time, error) {
	var raw sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(started_at) FROM sync_runs
		WHERE user_id = $1 AND kind = 'lastfm-similar' AND status != 'failed'
	`, userID).Scan(&raw)
	if err != nil {
		return false, time.Time{}, err
	}
	if !raw.Valid {
		return false, time.Time{}, nil
	}
	return now.Sub(raw.Time) < MinInterval, raw.Time, nil
}

func (s *Service) upsertArtist(ctx context.Context, mbid, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artists (mbid, name, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (mbid) DO UPDATE SET
		  name       = excluded.name,
		  updated_at = excluded.updated_at
	`, mbid, name, s.now().UTC())
	return err
}

func (s *Service) upsertAlbum(ctx context.Context, mbid, artistMBID, title, primaryType, releaseDate string) error {
	t := primaryType
	if t == "" {
		t = "Album"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO albums (mbid, primary_artist_mbid, title, release_date, type, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
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
		VALUES ($1, 'album', $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, subject_type, subject_id, source) DO UPDATE SET
		  raw_score    = excluded.raw_score,
		  reason_data  = excluded.reason_data,
		  expires_at   = excluded.expires_at
	`, userID, albumMBID, discover.SourceLastfmSim, score, reason, s.now().UTC(), expiresAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Service) startRun(ctx context.Context, userID, kind string, started time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO sync_runs (user_id, kind, started_at, status)
		VALUES ($1, $2, $3, 'running')
		RETURNING id
	`, userID, kind, started).Scan(&id)
	return id, err
}

func (s *Service) finishRun(ctx context.Context, id int64, status string, itemsAdded int, errText string, finished time.Time) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sync_runs
		SET status = $1, finished_at = $2, items_added = $3, error = NULLIF($4, '')
		WHERE id = $5
	`, status, finished, itemsAdded, errText, id)
	if err != nil {
		s.logger.Warn("lfsimilar: finishRun update failed", "err", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
