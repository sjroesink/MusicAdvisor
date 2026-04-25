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
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
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
	FetchTopTracks(ctx context.Context, accessToken string, tr spotify.TopTimeRange) ([]spotify.TopTrack, error)
	RefreshToken(ctx context.Context, refreshToken string) (spotify.TokenSet, error)
}

// Resolver is the minimal surface needed to map top items to MBIDs.
type Resolver interface {
	ResolveArtistByName(ctx context.Context, spotifyID, name string) (resolver.Result, error)
	ResolveTrack(ctx context.Context, spotifyID, isrc string) (resolver.Result, error)
	ResolveAlbum(ctx context.Context, spotifyID, upc string) (resolver.Result, error)
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
	RunID         int64
	Status        string // "ok" | "skipped" | "partial" | "failed"
	Ranges        int    // how many ranges were actually fetched this run
	ArtistsSeen   int
	TracksSeen    int
	ArtistsDone   int // artists that were snapshotted+signalled
	TracksDone    int
	Unresolved    int
	Errors        int
	DurationMs    int64
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
				if errors.Is(err, spotify.ErrInvalidGrant) {
					return "", "", time.Time{}, user.AsTerminal(err)
				}
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
		mult := rangeWeightMult[tr]

		s.logger.Info("fetching top artists", "user_id", userID, "range", tr)
		artists, err := s.spotify.FetchTopArtists(ctx, accessToken, tr)
		if err != nil {
			s.logger.Warn("fetch top artists failed", "user_id", userID, "range", tr, "err", err)
			result.Errors++
		} else {
			result.Ranges++
			result.ArtistsSeen += len(artists)
			for _, a := range artists {
				s.resolveOneArtist(ctx, userID, a, tr, mult, snapshotAt, &result)
			}
		}

		s.logger.Info("fetching top tracks", "user_id", userID, "range", tr)
		tracks, err := s.spotify.FetchTopTracks(ctx, accessToken, tr)
		if err != nil {
			s.logger.Warn("fetch top tracks failed", "user_id", userID, "range", tr, "err", err)
			result.Errors++
		} else {
			result.TracksSeen += len(tracks)
			for _, t := range tracks {
				s.resolveOneTrack(ctx, userID, t, tr, mult, snapshotAt, &result)
			}
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
	s.finishRun(ctx, runID, status, result.ArtistsDone+result.TracksDone, "", finished)
	result.Status = status
	result.DurationMs = finished.Sub(started).Milliseconds()
	s.logger.Info("toplists sync finished",
		"user_id", userID, "status", status,
		"ranges", result.Ranges,
		"artists_seen", result.ArtistsSeen, "artists_done", result.ArtistsDone,
		"tracks_seen", result.TracksSeen, "tracks_done", result.TracksDone,
		"unresolved", result.Unresolved, "errors", result.Errors,
		"duration_s", result.DurationMs/1000,
	)
	return result, nil
}

func (s *Service) resolveOneArtist(ctx context.Context, userID string, a spotify.TopArtist,
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
	if err := s.insertSnapshot(ctx, userID, "artist", string(tr), a.Rank, res.MBID, snapshotAt); err != nil {
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
	r.ArtistsDone++
	s.logger.Debug("top artist scored",
		"range", tr, "rank", a.Rank, "mbid", res.MBID, "name", a.Name, "weight", weight,
	)
}

// resolveOneTrack resolves a top track to its MBID via ISRC. The track's
// album and artist get placeholder catalog rows if the resolver didn't
// already supply them — this keeps FK constraints on tracks happy and
// gives propagation a path to the artist_affinity table.
func (s *Service) resolveOneTrack(ctx context.Context, userID string, t spotify.TopTrack,
	tr spotify.TopTimeRange, mult float64, snapshotAt time.Time, r *RunResult) {

	res, err := s.resolver.ResolveTrack(ctx, t.SpotifyID, t.ISRC)
	if errors.Is(err, resolver.ErrUnresolved) {
		r.Unresolved++
		return
	}
	if err != nil {
		s.logger.Warn("resolve top track failed", "spotify_id", t.SpotifyID, "err", err)
		r.Errors++
		return
	}

	// Ensure the track's artist row exists before the track FK fires. For
	// tracks with an ArtistMBID in the resolver result we have an MBID; for
	// artists that only came from Spotify we fall back to the placeholder
	// scheme library sync uses ("sp:<spotify_id>").
	artistMBID := res.ArtistMBID
	artistName := firstNonEmpty(res.ArtistName, t.ArtistName)
	if artistMBID == "" && t.ArtistID != "" {
		artistMBID = "sp:" + t.ArtistID
	}
	if artistMBID != "" && artistName != "" {
		if err := s.upsertArtist(ctx, artistMBID, t.ArtistID, artistName); err != nil {
			r.Errors++
			return
		}
	}

	// Album placeholder so tracks.album_mbid FK is satisfied. The album
	// MBID from resolver-of-track is a release-group ID.
	albumMBID := res.ReleaseGroupID
	if albumMBID == "" && t.AlbumID != "" {
		albumMBID = "sp:" + t.AlbumID
	}
	if albumMBID != "" {
		if err := s.upsertAlbumPlaceholder(ctx, albumMBID, t.AlbumID, t.AlbumName, artistMBID); err != nil {
			r.Errors++
			return
		}
	}

	if err := s.upsertTrack(ctx, res.MBID, t.SpotifyID, t.Name, t.DurationMs, albumMBID, artistMBID); err != nil {
		r.Errors++
		return
	}
	if err := s.insertSnapshot(ctx, userID, "track", string(tr), t.Rank, res.MBID, snapshotAt); err != nil {
		r.Errors++
		return
	}
	weight := topRankWeight(t.Rank, mult)
	if err := s.signals.Append(ctx, signal.Event{
		UserID:      userID,
		Kind:        signal.TopRank,
		SubjectType: signal.SubjectTrack,
		SubjectID:   res.MBID,
		Source:      signal.SourceTop,
		Weight:      weight,
		Context:     fmt.Sprintf(`{"range":%q,"rank":%d}`, string(tr), t.Rank),
		Timestamp:   snapshotAt,
	}); err != nil {
		r.Errors++
		return
	}
	r.TracksDone++
	s.logger.Debug("top track scored",
		"range", tr, "rank", t.Rank, "mbid", res.MBID, "title", t.Name, "weight", weight,
	)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
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

// hasFreshSnapshot returns the most recent top-snapshot timestamp and a
// boolean indicating whether it falls inside the MinInterval gate.
func (s *Service) hasFreshSnapshot(ctx context.Context, userID string, now time.Time) (bool, time.Time, error) {
	var raw sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT MAX(snapshot_at) FROM top_snapshots
		WHERE user_id = $1 AND kind = 'artist'
	`, userID).Scan(&raw)
	if err != nil {
		return false, time.Time{}, err
	}
	if !raw.Valid {
		return false, time.Time{}, nil
	}
	return now.Sub(raw.Time) < MinInterval, raw.Time, nil
}

func (s *Service) upsertArtist(ctx context.Context, mbid, spotifyID, name string) error {
	if mbid == "" {
		return errors.New("upsertArtist: mbid required")
	}
	// artists.spotify_id has a UNIQUE constraint; when a different mbid
	// already holds this spotify_id (e.g. an earlier `sp:…` placeholder),
	// ON CONFLICT (mbid) won't catch it. Update that row instead.
	if spotifyID != "" {
		res, err := s.db.ExecContext(ctx, `
			UPDATE artists SET name = $2, updated_at = $3
			WHERE spotify_id = $1 AND mbid <> $4
		`, spotifyID, name, s.now().UTC(), mbid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artists (mbid, spotify_id, name, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (mbid) DO UPDATE SET
		  spotify_id = COALESCE(excluded.spotify_id, artists.spotify_id),
		  name       = excluded.name,
		  updated_at = excluded.updated_at
	`, mbid, nullIfEmpty(spotifyID), name, s.now().UTC())
	return err
}

func (s *Service) insertSnapshot(ctx context.Context, userID, kind, timeRange string, rank int, mbid string, snapshotAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO top_snapshots (user_id, kind, time_range, rank, subject_mbid, snapshot_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, userID, kind, timeRange, rank, mbid, snapshotAt)
	return err
}

func (s *Service) upsertAlbumPlaceholder(ctx context.Context, mbid, spotifyID, title, artistMBID string) error {
	if spotifyID != "" {
		res, err := s.db.ExecContext(ctx, `
			UPDATE albums
			SET title = $2,
			    primary_artist_mbid = COALESCE($3, primary_artist_mbid),
			    updated_at = $4
			WHERE spotify_id = $1 AND mbid <> $5
		`, spotifyID, title, nullIfEmpty(artistMBID), s.now().UTC(), mbid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO albums (mbid, spotify_id, primary_artist_mbid, title, type, updated_at)
		VALUES ($1, $2, $3, $4, 'Album', $5)
		ON CONFLICT (mbid) DO UPDATE SET
		  spotify_id          = COALESCE(excluded.spotify_id, albums.spotify_id),
		  primary_artist_mbid = COALESCE(excluded.primary_artist_mbid, albums.primary_artist_mbid),
		  title               = excluded.title,
		  updated_at          = excluded.updated_at
	`, mbid, nullIfEmpty(spotifyID), nullIfEmpty(artistMBID), title, s.now().UTC())
	return err
}

func (s *Service) upsertTrack(ctx context.Context, mbid, spotifyID, title string, durationMs int, albumMBID, artistMBID string) error {
	var duration any
	if durationMs > 0 {
		duration = durationMs / 1000
	}
	if spotifyID != "" {
		res, err := s.db.ExecContext(ctx, `
			UPDATE tracks
			SET title        = $2,
			    album_mbid   = COALESCE($3, album_mbid),
			    artist_mbid  = COALESCE($4, artist_mbid),
			    duration_sec = COALESCE($5, duration_sec),
			    updated_at   = $6
			WHERE spotify_id = $1 AND mbid <> $7
		`, spotifyID, title, nullIfEmpty(albumMBID), nullIfEmpty(artistMBID), duration, s.now().UTC(), mbid)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tracks (mbid, spotify_id, album_mbid, artist_mbid, title, duration_sec, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (mbid) DO UPDATE SET
		  spotify_id   = COALESCE(excluded.spotify_id, tracks.spotify_id),
		  album_mbid   = COALESCE(excluded.album_mbid, tracks.album_mbid),
		  artist_mbid  = COALESCE(excluded.artist_mbid, tracks.artist_mbid),
		  title        = excluded.title,
		  duration_sec = COALESCE(excluded.duration_sec, tracks.duration_sec),
		  updated_at   = excluded.updated_at
	`, mbid, nullIfEmpty(spotifyID), nullIfEmpty(albumMBID), nullIfEmpty(artistMBID), title, duration, s.now().UTC())
	return err
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
		s.logger.Warn("toplists: finishRun update failed", "err", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
