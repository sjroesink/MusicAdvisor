// Package samelabel writes discover candidates surfaced by the MusicBrainz
// label graph. For each seed artist we collect the labels of their recent
// release-groups, then browse other release-groups on those labels. The
// intent is catalog discovery along curated lines ("you like artists on
// XL Recordings; here's someone else on XL").
//
// Scoring: raw_score = (1 + seed_affinity/10) × recency_decay(days_old).
// Labels themselves don't carry an MB confidence score, so we lean on the
// seed artist's affinity plus release recency.
package samelabel

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
	// MinInterval matches the other MB-driven sources (6h).
	MinInterval = 6 * time.Hour
	// MaxSeedArtists caps how many seeds we walk. Labels tend to fan out
	// aggressively so 15 seeds is already plenty of MB traffic.
	MaxSeedArtists = 15
	// MaxLabelsPerSeed caps distinct labels we follow per seed artist.
	MaxLabelsPerSeed = 3
	// MaxReleasesPerLabel caps candidates per label. Big labels would
	// otherwise flood the feed.
	MaxReleasesPerLabel = 3
	// SeedReleasesProbed is how many recent release-groups we inspect per
	// seed artist to discover their labels.
	SeedReleasesProbed = 3
	// RecencyWindow cuts off how old a label release can be. Labels move
	// slowly, so we go long.
	RecencyWindow = 365 * 24 * time.Hour
	// MinRecencyFloor keeps catalog titles alive near the window edge.
	MinRecencyFloor = 0.25
)

// CandidateTTL from the shared discover policy.
var CandidateTTL = discover.TTLForSource(discover.SourceMBSameLabel)

// MBClient is the narrow subset of musicbrainz.Client we depend on.
type MBClient interface {
	BrowseReleaseGroupsByArtist(ctx context.Context, artistMBID string, limit int) ([]musicbrainz.ReleaseGroup, error)
	ReleaseGroupLabels(ctx context.Context, releaseGroupMBID string) ([]musicbrainz.Label, error)
	BrowseReleaseGroupsByLabel(ctx context.Context, labelMBID string, limit int) ([]musicbrainz.ReleaseGroup, error)
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
	LabelsDiscovered  int
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
		result.SkippedReason = "last mb-same-label run at " + latest.Format(time.RFC3339)
		s.logger.Info("samelabel skipped (fresh run)", "user_id", userID, "latest", latest)
		return result, nil
	}

	runID, err := s.startRun(ctx, userID, "mb-same-label", started)
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

	excluded, err := s.loadAlbumExclusions(ctx, userID)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, err.Error(), s.now().UTC())
		result.Status = "failed"
		return result, err
	}

	cutoff := started.Add(-RecencyWindow)
	expires := started.Add(CandidateTTL)
	// Labels we've already processed this run — avoids re-scanning the same
	// label when multiple seeds share it.
	processedLabels := map[string]bool{}

	for _, seed := range seeds {
		result.SeedsScanned++
		labels, err := s.collectLabels(ctx, seed.MBID)
		if err != nil {
			s.logger.Warn("samelabel: label lookup failed", "seed", seed.MBID, "err", err)
			result.Errors++
			continue
		}

		pickedLabels := 0
		for _, lbl := range labels {
			if pickedLabels >= MaxLabelsPerSeed {
				break
			}
			if processedLabels[lbl.MBID] {
				continue
			}
			processedLabels[lbl.MBID] = true
			pickedLabels++
			result.LabelsDiscovered++

			_ = s.upsertLabel(ctx, lbl.MBID, lbl.Name)

			rgs, err := s.mb.BrowseReleaseGroupsByLabel(ctx, lbl.MBID, MaxReleasesPerLabel*5)
			if err != nil {
				s.logger.Warn("samelabel: browse-by-label failed", "label", lbl.MBID, "err", err)
				result.Errors++
				continue
			}

			picked := 0
			for _, rg := range rgs {
				if picked >= MaxReleasesPerLabel {
					break
				}
				if excluded[rg.MBID] {
					continue
				}
				releaseDate, ok := parseMBDate(rg.FirstReleaseDate)
				if !ok || releaseDate.Before(cutoff) {
					continue
				}
				daysOld := started.Sub(releaseDate).Hours() / 24
				score := (1 + seed.Score/10) * recencyDecay(daysOld)
				reason := fmt.Sprintf(
					`{"via_artist_mbid":%q,"via_artist_name":%q,"label_mbid":%q,"label_name":%q,"seed_affinity":%f,"release_date":%q}`,
					seed.MBID, seed.Name, lbl.MBID, lbl.Name, seed.Score, rg.FirstReleaseDate,
				)
				if rg.ArtistName != "" && rg.ArtistID != "" {
					_ = s.upsertArtist(ctx, rg.ArtistID, rg.ArtistName)
				}
				if err := s.upsertAlbum(ctx, rg.MBID, rg.ArtistID, rg.Title, rg.PrimaryType, rg.FirstReleaseDate); err != nil {
					result.Errors++
					continue
				}
				_ = s.linkAlbumLabel(ctx, rg.MBID, lbl.MBID)
				inserted, err := s.upsertCandidate(ctx, userID, rg.MBID, score, reason, expires)
				if err != nil {
					s.logger.Warn("samelabel: candidate write failed", "mbid", rg.MBID, "err", err)
					result.Errors++
					continue
				}
				if inserted {
					result.CandidatesNew++
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
	s.logger.Info("samelabel sync finished",
		"user_id", userID, "status", status,
		"seeds", result.SeedsScanned,
		"labels", result.LabelsDiscovered,
		"new", result.CandidatesNew, "updated", result.CandidatesUpdated,
		"errors", result.Errors, "duration_s", result.DurationMs/1000,
	)
	return result, nil
}

// collectLabels finds which labels the seed artist released on, preferring
// already-known `album_labels` rows when we've seen them before and falling
// back to live MB lookups for the most recent SeedReleasesProbed releases.
func (s *Service) collectLabels(ctx context.Context, artistMBID string) ([]musicbrainz.Label, error) {
	// 1) Cached labels from previous runs' album_labels join.
	seen := map[string]musicbrainz.Label{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT l.mbid, l.name
		FROM album_labels al
		JOIN labels l  ON l.mbid = al.label_mbid
		JOIN albums a  ON a.mbid = al.album_mbid
		WHERE a.primary_artist_mbid = ?
	`, artistMBID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l musicbrainz.Label
		if err := rows.Scan(&l.MBID, &l.Name); err != nil {
			rows.Close()
			return nil, err
		}
		seen[l.MBID] = l
	}
	rows.Close()
	if len(seen) > 0 {
		out := make([]musicbrainz.Label, 0, len(seen))
		for _, l := range seen {
			out = append(out, l)
		}
		return out, nil
	}

	// 2) Live MB lookup: most recent SeedReleasesProbed release-groups.
	rgs, err := s.mb.BrowseReleaseGroupsByArtist(ctx, artistMBID, 10)
	if err != nil {
		return nil, err
	}
	// Keep newest first; MB's order is not guaranteed.
	if len(rgs) > SeedReleasesProbed {
		rgs = rgs[:SeedReleasesProbed]
	}
	for _, rg := range rgs {
		labels, err := s.mb.ReleaseGroupLabels(ctx, rg.MBID)
		if err != nil {
			s.logger.Warn("samelabel: label lookup failed", "rg", rg.MBID, "err", err)
			continue
		}
		for _, l := range labels {
			if _, ok := seen[l.MBID]; !ok {
				seen[l.MBID] = l
			}
		}
	}
	out := make([]musicbrainz.Label, 0, len(seen))
	for _, l := range seen {
		out = append(out, l)
	}
	return out, nil
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

// ── DB helpers ──

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

// loadAlbumExclusions returns the set of album MBIDs the user already has
// saved or explicitly hidden. Same-label shouldn't re-surface either.
func (s *Service) loadAlbumExclusions(ctx context.Context, userID string) (map[string]bool, error) {
	out := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT album_mbid FROM saved_albums WHERE user_id = ?`, userID)
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
		SELECT subject_id FROM hides WHERE user_id = ? AND subject_type = 'album'
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
		WHERE user_id = ? AND kind = 'mb-same-label' AND status != 'failed'
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
	return time.Time{}, errors.New("samelabel: unrecognized time format " + s)
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

func (s *Service) upsertLabel(ctx context.Context, mbid, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO labels (mbid, name) VALUES (?, ?)
		ON CONFLICT (mbid) DO UPDATE SET name = excluded.name
	`, mbid, name)
	return err
}

func (s *Service) linkAlbumLabel(ctx context.Context, albumMBID, labelMBID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO album_labels (album_mbid, label_mbid) VALUES (?, ?)
		ON CONFLICT DO NOTHING
	`, albumMBID, labelMBID)
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
	`, userID, albumMBID, discover.SourceMBSameLabel, score, reason, s.now().UTC(), expiresAt)
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
		s.logger.Warn("samelabel: finishRun update failed", "err", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
