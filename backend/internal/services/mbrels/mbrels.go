// Package mbrels writes discover candidates sourced from MusicBrainz
// artist-to-artist relations. For each top-affinity seed artist we walk
// `artist/{mbid}?inc=artist-rels` and, for the other end of each edge,
// surface one or two recent release-groups. The intent is catalog
// discovery ("you like band X; its drummer has a solo project Y").
//
// Scoring: raw_score = relation_strength × (1 + seed_affinity/10) ×
// recency_decay(days_old). Relation strengths come from
// musicbrainz.RelationStrength; "member of band" outranks
// "supporting musician", which outranks "performer".
package mbrels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/discover"
)

const (
	// MinInterval rate-limits the whole pass. 6h matches the new-releases
	// cadence; artist-rels edges don't change minute-to-minute.
	MinInterval = 6 * time.Hour
	// MaxSeedArtists caps how many seeds we branch off of. Each seed does
	// one MB artist-rels call + (up to MaxRelatedPerSeed) browse calls.
	MaxSeedArtists = 20
	// MaxRelatedPerSeed caps how many related artists we act on per seed.
	MaxRelatedPerSeed = 4
	// ReleasesPerRelated is how many recent release-groups we surface per
	// discovered related artist.
	ReleasesPerRelated = 2
	// RecencyWindow is the cut-off for "worth surfacing". Catalog discovery
	// tolerates older releases than /mb-new-releases; 180 days keeps slow-
	// discovery candidates alive without dredging up ancient back-catalog.
	RecencyWindow = 180 * 24 * time.Hour
	// MinRecencyFloor is the smallest recency-decay we emit. Anything
	// within the window scores at least MinRecencyFloor × other factors.
	MinRecencyFloor = 0.3
)

// CandidateTTL lives in the discover package so policies stay centralised.
var CandidateTTL = discover.TTLForSource(discover.SourceMBArtistRel)

// MBClient is the narrow subset of musicbrainz.Client that mbrels uses.
// Kept small so tests can mock without pulling the whole HTTP stack.
type MBClient interface {
	FetchArtistRelations(ctx context.Context, mbid string) ([]musicbrainz.ArtistRelation, error)
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
	RunID             int64
	Status            string
	SeedsScanned      int
	RelatedDiscovered int
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
		result.SkippedReason = "last mb-artist-rels run at " + latest.Format(time.RFC3339)
		s.logger.Info("mbrels skipped (fresh run)", "user_id", userID, "latest", latest)
		return result, nil
	}

	runID, err := s.startRun(ctx, userID, "mb-artist-rels", started)
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
		s.logger.Info("mbrels: no seed artists", "user_id", userID)
		return result, nil
	}

	excluded, err := s.loadExclusions(ctx, userID)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, err.Error(), s.now().UTC())
		result.Status = "failed"
		return result, err
	}

	cutoff := started.Add(-RecencyWindow)
	expires := started.Add(CandidateTTL)

	for _, seed := range seeds {
		result.SeedsScanned++
		rels, err := s.mb.FetchArtistRelations(ctx, seed.MBID)
		if err != nil {
			s.logger.Warn("mbrels: relations fetch failed", "seed", seed.MBID, "err", err)
			result.Errors++
			continue
		}
		// Collapse duplicate targets — MB sometimes lists the same pair
		// under two relation types. Keep the strongest.
		byTarget := map[string]musicbrainz.ArtistRelation{}
		for _, r := range rels {
			if excluded[r.Target.MBID] {
				continue
			}
			prev, ok := byTarget[r.Target.MBID]
			if !ok || musicbrainz.RelationStrength(r.Type) > musicbrainz.RelationStrength(prev.Type) {
				byTarget[r.Target.MBID] = r
			}
		}
		// Sort candidates by relation-strength DESC to spend our MB budget
		// on the tightest edges first.
		ranked := make([]musicbrainz.ArtistRelation, 0, len(byTarget))
		for _, r := range byTarget {
			ranked = append(ranked, r)
		}
		sort.SliceStable(ranked, func(i, j int) bool {
			return musicbrainz.RelationStrength(ranked[i].Type) >
				musicbrainz.RelationStrength(ranked[j].Type)
		})
		if len(ranked) > MaxRelatedPerSeed {
			ranked = ranked[:MaxRelatedPerSeed]
		}

		for _, r := range ranked {
			result.RelatedDiscovered++
			rgs, err := s.mb.BrowseReleaseGroupsByArtist(ctx, r.Target.MBID, 15)
			if err != nil {
				s.logger.Warn("mbrels: browse failed", "related", r.Target.MBID, "err", err)
				result.Errors++
				continue
			}
			sort.SliceStable(rgs, func(i, j int) bool {
				return rgs[i].FirstReleaseDate > rgs[j].FirstReleaseDate
			})

			if r.Target.Name != "" {
				_ = s.upsertArtist(ctx, r.Target.MBID, r.Target.Name)
			}

			picked := 0
			for _, rg := range rgs {
				if picked >= ReleasesPerRelated {
					break
				}
				releaseDate, ok := parseMBDate(rg.FirstReleaseDate)
				if !ok || releaseDate.Before(cutoff) {
					continue
				}
				daysOld := started.Sub(releaseDate).Hours() / 24
				recency := recencyDecay(daysOld)
				strength := musicbrainz.RelationStrength(r.Type)
				score := strength * (1 + seed.Score/10) * recency
				reason := fmt.Sprintf(
					`{"via_artist_mbid":%q,"via_artist_name":%q,"relation":%q,"relation_strength":%f,"seed_affinity":%f,"release_date":%q}`,
					seed.MBID, seed.Name, r.Type, strength, seed.Score, rg.FirstReleaseDate,
				)
				if err := s.upsertAlbum(ctx, rg.MBID, r.Target.MBID, rg.Title, rg.PrimaryType, rg.FirstReleaseDate); err != nil {
					result.Errors++
					continue
				}
				inserted, err := s.upsertCandidate(ctx, userID, rg.MBID, score, reason, expires)
				if err != nil {
					s.logger.Warn("mbrels: candidate write failed", "mbid", rg.MBID, "err", err)
					result.Errors++
					continue
				}
				if inserted {
					result.CandidatesNew++
					s.logger.Debug("mbrels candidate",
						"seed", seed.Name, "related", r.Target.Name,
						"type", r.Type, "rg", rg.Title, "score", score)
				} else {
					result.CandidatesUpdated++
				}
				picked++
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
	s.logger.Info("mbrels sync finished",
		"user_id", userID, "status", status,
		"seeds", result.SeedsScanned,
		"related", result.RelatedDiscovered,
		"new", result.CandidatesNew, "updated", result.CandidatesUpdated,
		"errors", result.Errors, "duration_s", result.DurationMs/1000,
	)
	return result, nil
}

func recencyDecay(daysOld float64) float64 {
	if daysOld < 0 {
		daysOld = 0
	}
	windowDays := RecencyWindow.Hours() / 24
	w := 1 - (daysOld / windowDays)
	if w < MinRecencyFloor {
		return MinRecencyFloor
	}
	return w
}

func parseMBDate(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// ── DB helpers (local copies — see lbsimilar/releases for the same shape) ──

func (s *Service) loadSeeds(ctx context.Context, userID string, limit int) ([]seedArtist, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.mbid, a.name, COALESCE(aa.score, 0) AS score
		FROM artists a
		LEFT JOIN artist_affinity aa
		       ON aa.artist_mbid = a.mbid AND aa.user_id = ?
		WHERE a.mbid IN (SELECT artist_mbid FROM saved_artists WHERE user_id = ?)
		   OR COALESCE(aa.score, 0) > 0
		ORDER BY score DESC, a.name
		LIMIT ?
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
	rows, err := s.db.QueryContext(ctx, `SELECT artist_mbid FROM saved_artists WHERE user_id = ?`, userID)
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
		SELECT subject_id FROM hides WHERE user_id = ? AND subject_type = 'artist'
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
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(started_at) FROM sync_runs
		WHERE user_id = ? AND kind = 'mb-artist-rels' AND status != 'failed'
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
	return time.Time{}, errors.New("mbrels: unrecognized time format " + s)
}

func (s *Service) upsertArtist(ctx context.Context, mbid, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artists (mbid, name, updated_at)
		VALUES (?, ?, ?)
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
		VALUES (?, 'album', ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, subject_type, subject_id, source) DO UPDATE SET
		  raw_score    = excluded.raw_score,
		  reason_data  = excluded.reason_data,
		  expires_at   = excluded.expires_at
	`, userID, albumMBID, discover.SourceMBArtistRel, score, reason, s.now().UTC(), expiresAt)
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
		s.logger.Warn("mbrels: finishRun update failed", "err", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
