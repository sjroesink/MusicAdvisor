// Package listening polls Spotify's /me/player/recently-played, writes
// play_history rows, and derives play_full / play_skip signals.
//
// Skip detection (spec §5.1): given two adjacent plays P (earlier) and
// N (later) in chronological order, if
//
//	P.played_at + P.duration_sec - 30s > N.played_at
//
// then P was skipped: the user moved to the next track before the previous
// finished (minus a 30s tolerance for late API reporting). Otherwise P
// played in full.
//
// We emit a signal for P only when N is a freshly-inserted play in this
// poll. That guarantees exactly-once emission: the last play in any poll
// has no known "next" yet and stays unemitted until the next poll shows a
// successor. Re-running the same poll inserts nothing new and emits
// nothing, so manual /api/sync/trigger retries are safe.
package listening

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
)

// SkipTolerance is the grace window between "paused/skipped" and "played
// in full". 30s matches the spec and absorbs late recently-played reporting.
const SkipTolerance = 30 * time.Second

type TokenProvider interface {
	AccessToken(ctx context.Context, userID, provider string,
		refresh func(ctx context.Context, externalID, refreshToken string) (string, string, time.Time, error),
	) (string, error)
}

type SpotifyClient interface {
	FetchRecentlyPlayed(ctx context.Context, accessToken string, after time.Time) ([]spotify.RecentPlay, error)
	RefreshToken(ctx context.Context, refreshToken string) (spotify.TokenSet, error)
}

type Resolver interface {
	ResolveTrack(ctx context.Context, spotifyID, isrc string) (resolver.Result, error)
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

type RunResult struct {
	RunID      int64
	Status     string // "ok" | "partial" | "failed"
	Fetched    int
	Inserted   int
	Unresolved int
	Skipped    int
	PlayedFull int
	Errors     int
	DurationMs int64
}

// play is the internal representation of one entry in the chain. Fresh
// plays get emission; OLD plays (the persisted last-known) do not.
type play struct {
	mbid        string // may be "" if unresolved — no emission in that case
	spotifyID   string
	name        string
	durationSec int
	playedAt    time.Time
	isNew       bool
}

// Sync polls /me/player/recently-played once and writes resulting rows +
// signals. Not idempotent against Spotify (new plays since last poll will
// still arrive) but idempotent against replay within a poll window.
func (s *Service) Sync(ctx context.Context, userID string) (RunResult, error) {
	started := s.now().UTC()
	result := RunResult{}
	runID, err := s.startRun(ctx, userID, "spotify-recent", started)
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

	// Cursor: only ask Spotify for plays newer than the latest we've seen.
	// Zero time means "give me the most recent window" on first poll.
	after, lastKnown, err := s.loadLastKnownPlay(ctx, userID)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, err.Error(), s.now().UTC())
		result.Status = "failed"
		return result, err
	}

	recents, err := s.spotify.FetchRecentlyPlayed(ctx, accessToken, after)
	if err != nil {
		s.finishRun(ctx, runID, "failed", 0, err.Error(), s.now().UTC())
		result.Status = "failed"
		return result, err
	}
	result.Fetched = len(recents)

	// Chronological order: oldest first. Skip detection pairs (earlier, later).
	sort.SliceStable(recents, func(i, j int) bool {
		return recents[i].PlayedAt.Before(recents[j].PlayedAt)
	})

	chain := make([]play, 0, len(recents)+1)
	if lastKnown != nil {
		chain = append(chain, *lastKnown)
	}

	for _, rp := range recents {
		p, inserted, err := s.ingestPlay(ctx, userID, rp, &result)
		if err != nil {
			s.logger.Warn("ingest play failed", "spotify_id", rp.SpotifyID, "err", err)
			result.Errors++
			continue
		}
		if !inserted {
			// Already in play_history (seen in a previous poll). Include in
			// chain only if it's the latest lastKnown; otherwise skip to avoid
			// double-emission.
			continue
		}
		p.isNew = true
		chain = append(chain, p)
		result.Inserted++
	}

	for i := 0; i+1 < len(chain); i++ {
		earlier, later := chain[i], chain[i+1]
		if !later.isNew {
			continue
		}
		if earlier.mbid == "" {
			continue // unresolved: can't attach signal to an MBID
		}
		s.emitFullOrSkip(ctx, userID, earlier, later, &result)
	}

	status := "ok"
	if result.Errors > 0 || result.Unresolved > 0 {
		status = "partial"
	}
	finished := s.now().UTC()
	s.finishRun(ctx, runID, status, result.Inserted, "", finished)
	result.Status = status
	result.DurationMs = finished.Sub(started).Milliseconds()
	s.logger.Info("listening sync finished",
		"user_id", userID, "status", status,
		"fetched", result.Fetched, "inserted", result.Inserted,
		"unresolved", result.Unresolved, "full", result.PlayedFull, "skipped", result.Skipped,
		"errors", result.Errors, "duration_ms", result.DurationMs,
	)
	return result, nil
}

func (s *Service) emitFullOrSkip(ctx context.Context, userID string, earlier, later play, r *RunResult) {
	var kind signal.Kind
	var contextJSON string
	// earlier.played_at + earlier.duration - 30s > later.played_at → skip
	threshold := earlier.playedAt.Add(time.Duration(earlier.durationSec)*time.Second - SkipTolerance)
	if threshold.After(later.playedAt) {
		kind = signal.PlaySkip
		contextJSON = fmt.Sprintf(`{"confidence":0.8,"next_played_at":%q}`, later.playedAt.Format(time.RFC3339))
	} else {
		kind = signal.PlayFull
		contextJSON = fmt.Sprintf(`{"next_played_at":%q}`, later.playedAt.Format(time.RFC3339))
	}
	if err := s.signals.Append(ctx, signal.Event{
		UserID:      userID,
		Kind:        kind,
		SubjectType: signal.SubjectTrack,
		SubjectID:   earlier.mbid,
		Source:      signal.SourceRecentDerived,
		Context:     contextJSON,
		Timestamp:   earlier.playedAt,
	}); err != nil {
		s.logger.Warn("emit play signal failed", "kind", kind, "mbid", earlier.mbid, "err", err)
		r.Errors++
		return
	}
	if kind == signal.PlaySkip {
		r.Skipped++
	} else {
		r.PlayedFull++
	}
}

// ingestPlay resolves the track, upserts catalog rows as needed, and writes
// play_history. Returns the in-memory play, whether the row was newly
// inserted (ON CONFLICT DO NOTHING), and any non-recoverable DB error.
func (s *Service) ingestPlay(ctx context.Context, userID string, rp spotify.RecentPlay, r *RunResult) (play, bool, error) {
	p := play{
		spotifyID:   rp.SpotifyID,
		name:        rp.Name,
		durationSec: rp.DurationMs / 1000,
		playedAt:    rp.PlayedAt,
	}
	res, err := s.resolver.ResolveTrack(ctx, rp.SpotifyID, rp.ISRC)
	if errors.Is(err, resolver.ErrUnresolved) {
		r.Unresolved++
		// Record the play anyway — the raw history is still useful for feed
		// display and later re-resolution.
	} else if err != nil {
		return p, false, err
	} else {
		p.mbid = res.MBID
		// Upsert artist + album placeholder so tracks.mbid FK stays sound.
		artistMBID := firstNonEmpty(res.ArtistMBID, "sp:"+rp.ArtistID)
		artistName := firstNonEmpty(res.ArtistName, rp.ArtistName)
		if artistMBID != "" && artistName != "" {
			if err := s.upsertArtist(ctx, artistMBID, rp.ArtistID, artistName); err != nil {
				return p, false, err
			}
		}
		albumMBID := firstNonEmpty(res.ReleaseGroupID, "sp:"+rp.AlbumID)
		if albumMBID != "" {
			if err := s.upsertAlbumPlaceholder(ctx, albumMBID, rp.AlbumID, rp.AlbumName, artistMBID); err != nil {
				return p, false, err
			}
		}
		if err := s.upsertTrack(ctx, p.mbid, rp.SpotifyID, rp.Name, rp.DurationMs, albumMBID, artistMBID); err != nil {
			return p, false, err
		}
	}

	inserted, err := s.insertPlayHistory(ctx, userID, p.mbid, rp)
	if err != nil {
		return p, false, err
	}
	return p, inserted, nil
}

// ── DB ──────────────────────────────────────────────────────────────

func (s *Service) loadLastKnownPlay(ctx context.Context, userID string) (time.Time, *play, error) {
	// One query: last play_history row for user (MAX played_at), joined with
	// tracks to get duration. If no row exists, return zero time and nil
	// play — this is the "first poll ever" case.
	row := s.db.QueryRowContext(ctx, `
		SELECT ph.played_at, ph.track_mbid, COALESCE(t.duration_sec, 0)
		FROM play_history ph
		LEFT JOIN tracks t ON t.mbid = ph.track_mbid
		WHERE ph.user_id = $1
		ORDER BY ph.played_at DESC
		LIMIT 1
	`, userID)
	var playedAtRaw sql.NullTime
	var mbid sql.NullString
	var duration int
	if err := row.Scan(&playedAtRaw, &mbid, &duration); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil, nil
		}
		return time.Time{}, nil, err
	}
	if !playedAtRaw.Valid {
		return time.Time{}, nil, nil
	}
	playedAt := playedAtRaw.Time
	return playedAt, &play{
		mbid:        mbid.String,
		durationSec: duration,
		playedAt:    playedAt,
		isNew:       false,
	}, nil
}

func (s *Service) insertPlayHistory(ctx context.Context, userID, mbid string, rp spotify.RecentPlay) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO play_history
		  (user_id, track_mbid, spotify_track_id, played_at, source, context_uri)
		VALUES ($1, $2, $3, $4, 'recently-played', $5)
		ON CONFLICT DO NOTHING
	`, userID, nullIfEmpty(mbid), rp.SpotifyID, rp.PlayedAt, nullIfEmpty(rp.ContextURI))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// upsertArtist writes catalog rows from a Spotify recent-play. The table has
// both a primary-key constraint on `mbid` and a UNIQUE on `spotify_id`; the
// ON CONFLICT clause only handles mbid collisions. When the same spotify_id
// already lives on a different mbid (typically a `sp:…` placeholder row
// written earlier, before the resolver had a real MBID), we'd otherwise
// explode with SQLSTATE 23505. Pre-update the existing row and short-circuit
// when that happens.
func (s *Service) upsertArtist(ctx context.Context, mbid, spotifyID, name string) error {
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

func (s *Service) upsertAlbumPlaceholder(ctx context.Context, mbid, spotifyID, title, artistMBID string) error {
	// See upsertArtist for the rationale behind this pre-update path.
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
	// See upsertArtist for the rationale behind this pre-update path.
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
		s.logger.Warn("listening: finishRun update failed", "err", err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
