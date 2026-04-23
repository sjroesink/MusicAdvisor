// Package library performs a full Spotify library sync for one user:
// fetches saved albums / tracks / followed artists, resolves each to a
// MusicBrainz ID, upserts catalog + saved_* rows, and emits the first-class
// library_add / follow_add signals that later phases build on.
//
// A sync is long-running (MusicBrainz is 1 req/s) so orchestrators should
// run this in a goroutine and report progress through the sync_runs table.
// Errors on individual items are logged and counted but don't abort the
// overall run — partial success is encoded as sync_runs.status = 'partial'.
package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
)

// TokenProvider decouples the sync from the user service. Given a user ID
// and provider name, it returns a fresh access token (handling refresh if
// needed). Library sync is provider-specific but stays test-friendly.
type TokenProvider interface {
	AccessToken(ctx context.Context, userID, provider string, refresh func(ctx context.Context, externalID, refreshToken string) (string, string, time.Time, error)) (string, error)
}

// SpotifyClient is the slice of *spotify.Client we need.
type SpotifyClient interface {
	FetchSavedAlbums(ctx context.Context, accessToken string, onAlbum func(spotify.SavedAlbum) error) (int, error)
	FetchSavedTracks(ctx context.Context, accessToken string, onTrack func(spotify.SavedTrack) error) (int, error)
	FetchFollowedArtists(ctx context.Context, accessToken string, onArtist func(spotify.FollowedArtist) error) (int, error)
	RefreshToken(ctx context.Context, refreshToken string) (spotify.TokenSet, error)
}

// Resolver is the minimal surface we need from the resolver service.
type Resolver interface {
	ResolveAlbum(ctx context.Context, spotifyID, upc string) (resolver.Result, error)
	ResolveTrack(ctx context.Context, spotifyID, isrc string) (resolver.Result, error)
	ResolveArtistByName(ctx context.Context, spotifyID, name string) (resolver.Result, error)
}

// Service runs library syncs.
type Service struct {
	db        *sql.DB
	tokens    TokenProvider
	spotify   SpotifyClient
	resolver  Resolver
	signals   signal.Writer
	logger    *slog.Logger
	now       func() time.Time
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
		db:       db,
		tokens:   tokens,
		spotify:  sp,
		resolver: res,
		signals:  sigs,
		logger:   logger,
		now:      time.Now,
	}
}

// RunResult summarizes what a single sync did.
type RunResult struct {
	RunID         int64
	Status        string // "ok" | "partial" | "failed"
	AlbumsAdded   int
	TracksAdded   int
	ArtistsAdded  int
	Unresolved    int
	Errors        int
	DurationMs    int64
}

// Sync performs a full library sync for user userID. Returns even on partial
// failure — callers check Status.
func (s *Service) Sync(ctx context.Context, userID string) (RunResult, error) {
	started := s.now().UTC()
	runID, err := s.startRun(ctx, userID, "spotify-library", started)
	if err != nil {
		return RunResult{}, err
	}
	result := RunResult{RunID: runID}

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

	// Followed artists first — albums/tracks often reference artist IDs we
	// can now match without a search (saved_artists row pre-populates them).
	aErr := s.syncFollowedArtists(ctx, userID, accessToken, &result)
	alErr := s.syncSavedAlbums(ctx, userID, accessToken, &result)
	tErr := s.syncSavedTracks(ctx, userID, accessToken, &result)

	status := "ok"
	var finalErr string
	if aErr != nil || alErr != nil || tErr != nil || result.Errors > 0 || result.Unresolved > 0 {
		status = "partial"
	}
	if aErr != nil {
		finalErr = appendErr(finalErr, "artists: "+aErr.Error())
	}
	if alErr != nil {
		finalErr = appendErr(finalErr, "albums: "+alErr.Error())
	}
	if tErr != nil {
		finalErr = appendErr(finalErr, "tracks: "+tErr.Error())
	}
	finished := s.now().UTC()
	itemsAdded := result.AlbumsAdded + result.TracksAdded + result.ArtistsAdded
	s.finishRun(ctx, runID, status, itemsAdded, finalErr, finished)
	result.Status = status
	result.DurationMs = finished.Sub(started).Milliseconds()
	return result, nil
}

func (s *Service) syncFollowedArtists(ctx context.Context, userID, token string, r *RunResult) error {
	_, err := s.spotify.FetchFollowedArtists(ctx, token, func(a spotify.FollowedArtist) error {
		res, rerr := s.resolver.ResolveArtistByName(ctx, a.SpotifyID, a.Name)
		if errors.Is(rerr, resolver.ErrUnresolved) {
			r.Unresolved++
			return nil
		}
		if rerr != nil {
			s.logger.Warn("follow sync: resolve failed", "spotify_id", a.SpotifyID, "err", rerr)
			r.Errors++
			return nil
		}
		if err := s.upsertArtist(ctx, res.MBID, a.SpotifyID, a.Name); err != nil {
			r.Errors++
			return nil
		}
		if err := s.insertSavedArtist(ctx, userID, res.MBID); err != nil {
			r.Errors++
			return nil
		}
		if err := s.signals.Append(ctx, signal.Event{
			UserID:      userID,
			Kind:        signal.FollowAdd,
			SubjectType: signal.SubjectArtist,
			SubjectID:   res.MBID,
			Source:      signal.SourceLibrary,
		}); err != nil {
			r.Errors++
			return nil
		}
		r.ArtistsAdded++
		return nil
	})
	return err
}

func (s *Service) syncSavedAlbums(ctx context.Context, userID, token string, r *RunResult) error {
	_, err := s.spotify.FetchSavedAlbums(ctx, token, func(a spotify.SavedAlbum) error {
		res, rerr := s.resolver.ResolveAlbum(ctx, a.SpotifyID, a.UPC)
		if errors.Is(rerr, resolver.ErrUnresolved) {
			r.Unresolved++
			return nil
		}
		if rerr != nil {
			s.logger.Warn("album sync: resolve failed", "spotify_id", a.SpotifyID, "err", rerr)
			r.Errors++
			return nil
		}
		// Side-channel: the resolver handed back the primary artist. Make
		// sure the artist exists before the album FK fires.
		artistMBID := res.ArtistMBID
		if artistMBID != "" && a.ArtistID != "" {
			if err := s.upsertArtist(ctx, artistMBID, a.ArtistID, res.ArtistName); err != nil {
				r.Errors++
				return nil
			}
		} else if artistMBID == "" && a.ArtistID != "" && a.ArtistName != "" {
			artistMBID = "sp:" + a.ArtistID
			// Store a placeholder mbid so the FK holds; the artist can be
			// upgraded later when the follow-sync or a retry resolves it.
			if err := s.upsertArtistPlaceholder(ctx, artistMBID, a.ArtistID, a.ArtistName); err != nil {
				r.Errors++
				return nil
			}
		}

		if err := s.upsertAlbum(ctx, res, a, artistMBID); err != nil {
			r.Errors++
			return nil
		}
		if err := s.insertSavedAlbum(ctx, userID, res.MBID, a.AddedAt); err != nil {
			r.Errors++
			return nil
		}
		if err := s.signals.Append(ctx, signal.Event{
			UserID:      userID,
			Kind:        signal.LibraryAdd,
			SubjectType: signal.SubjectAlbum,
			SubjectID:   res.MBID,
			Source:      signal.SourceLibrary,
		}); err != nil {
			r.Errors++
			return nil
		}
		r.AlbumsAdded++
		return nil
	})
	return err
}

func (s *Service) syncSavedTracks(ctx context.Context, userID, token string, r *RunResult) error {
	_, err := s.spotify.FetchSavedTracks(ctx, token, func(t spotify.SavedTrack) error {
		res, rerr := s.resolver.ResolveTrack(ctx, t.SpotifyID, t.ISRC)
		if errors.Is(rerr, resolver.ErrUnresolved) {
			r.Unresolved++
			return nil
		}
		if rerr != nil {
			s.logger.Warn("track sync: resolve failed", "spotify_id", t.SpotifyID, "err", rerr)
			r.Errors++
			return nil
		}
		// Ensure artist + album rows exist before inserting the track.
		artistMBID := res.ArtistMBID
		if artistMBID == "" && t.ArtistID != "" && t.ArtistName != "" {
			artistMBID = "sp:" + t.ArtistID
			if err := s.upsertArtistPlaceholder(ctx, artistMBID, t.ArtistID, t.ArtistName); err != nil {
				r.Errors++
				return nil
			}
		} else if artistMBID != "" && t.ArtistID != "" {
			_ = s.upsertArtist(ctx, artistMBID, t.ArtistID, res.ArtistName)
		}

		albumMBID := res.ReleaseGroupID
		if albumMBID == "" && t.AlbumID != "" {
			albumMBID = "sp:" + t.AlbumID
			if err := s.upsertAlbumPlaceholder(ctx, albumMBID, t.AlbumID, t.AlbumName, artistMBID); err != nil {
				r.Errors++
				return nil
			}
		} else if albumMBID != "" {
			// The track resolver handed us a real release-group MBID but
			// didn't necessarily sync that album separately (it may not be
			// saved). Upsert a minimal album row so the track FK holds;
			// the album-sync (or a later MB enrichment job) can fill it in.
			if err := s.upsertAlbumPlaceholder(ctx, albumMBID, t.AlbumID, t.AlbumName, artistMBID); err != nil {
				r.Errors++
				return nil
			}
		}

		if err := s.upsertTrack(ctx, res.MBID, t.SpotifyID, albumMBID, artistMBID, t.Name, t.DurationMs); err != nil {
			r.Errors++
			return nil
		}
		if err := s.insertSavedTrack(ctx, userID, res.MBID, t.AddedAt); err != nil {
			r.Errors++
			return nil
		}
		if err := s.signals.Append(ctx, signal.Event{
			UserID:      userID,
			Kind:        signal.LibraryAdd,
			SubjectType: signal.SubjectTrack,
			SubjectID:   res.MBID,
			Source:      signal.SourceLibrary,
		}); err != nil {
			r.Errors++
			return nil
		}
		r.TracksAdded++
		return nil
	})
	return err
}

// ── catalog upserts ──────────────────────────────────────────────────

func (s *Service) upsertArtist(ctx context.Context, mbid, spotifyID, name string) error {
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

func (s *Service) upsertArtistPlaceholder(ctx context.Context, placeholderMBID, spotifyID, name string) error {
	return s.upsertArtist(ctx, placeholderMBID, spotifyID, name)
}

func (s *Service) upsertAlbum(ctx context.Context, res resolver.Result, a spotify.SavedAlbum, artistMBID string) error {
	albumType := normalizeAlbumType(res.PrimaryType, a.AlbumType)
	releaseDate := firstNonEmpty(res.FirstReleaseDate, a.ReleaseDate)
	title := firstNonEmpty(res.Title, a.Name)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO albums (mbid, spotify_id, primary_artist_mbid, title,
		                    release_date, type, track_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (mbid) DO UPDATE SET
		  spotify_id          = COALESCE(excluded.spotify_id, albums.spotify_id),
		  primary_artist_mbid = COALESCE(excluded.primary_artist_mbid, albums.primary_artist_mbid),
		  title               = excluded.title,
		  release_date        = excluded.release_date,
		  type                = excluded.type,
		  track_count         = COALESCE(excluded.track_count, albums.track_count),
		  updated_at          = excluded.updated_at
	`, res.MBID, nullIfEmpty(a.SpotifyID), nullIfEmpty(artistMBID), title,
		nullIfEmpty(releaseDate), albumType, zeroAsNull(a.TotalTracks), s.now().UTC())
	return err
}

func (s *Service) upsertAlbumPlaceholder(ctx context.Context, placeholderMBID, spotifyID, title, artistMBID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO albums (mbid, spotify_id, primary_artist_mbid, title, type, updated_at)
		VALUES (?, ?, ?, ?, 'Album', ?)
		ON CONFLICT (mbid) DO UPDATE SET
		  spotify_id          = COALESCE(excluded.spotify_id, albums.spotify_id),
		  primary_artist_mbid = COALESCE(excluded.primary_artist_mbid, albums.primary_artist_mbid),
		  title               = excluded.title,
		  updated_at          = excluded.updated_at
	`, placeholderMBID, nullIfEmpty(spotifyID), nullIfEmpty(artistMBID), title, s.now().UTC())
	return err
}

func (s *Service) upsertTrack(ctx context.Context, mbid, spotifyID, albumMBID, artistMBID, name string, durationMs int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tracks (mbid, spotify_id, album_mbid, artist_mbid, title, duration_sec, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (mbid) DO UPDATE SET
		  spotify_id   = COALESCE(excluded.spotify_id, tracks.spotify_id),
		  album_mbid   = COALESCE(excluded.album_mbid, tracks.album_mbid),
		  artist_mbid  = COALESCE(excluded.artist_mbid, tracks.artist_mbid),
		  title        = excluded.title,
		  duration_sec = COALESCE(excluded.duration_sec, tracks.duration_sec),
		  updated_at   = excluded.updated_at
	`, mbid, nullIfEmpty(spotifyID), nullIfEmpty(albumMBID), nullIfEmpty(artistMBID),
		name, zeroAsNull(durationMs/1000), s.now().UTC())
	return err
}

// ── saved_* inserts ─────────────────────────────────────────────────

func (s *Service) insertSavedArtist(ctx context.Context, userID, artistMBID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO saved_artists (user_id, artist_mbid, saved_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, artistMBID, s.now().UTC())
	return err
}

func (s *Service) insertSavedAlbum(ctx context.Context, userID, albumMBID string, savedAt time.Time) error {
	if savedAt.IsZero() {
		savedAt = s.now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO saved_albums (user_id, album_mbid, saved_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, albumMBID, savedAt)
	return err
}

func (s *Service) insertSavedTrack(ctx context.Context, userID, trackMBID string, savedAt time.Time) error {
	if savedAt.IsZero() {
		savedAt = s.now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO saved_tracks (user_id, track_mbid, saved_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, trackMBID, savedAt)
	return err
}

// ── sync_runs ───────────────────────────────────────────────────────

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
		s.logger.Warn("sync: finishRun update failed", "err", err)
	}
}

// ── helpers ─────────────────────────────────────────────────────────

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func zeroAsNull(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func appendErr(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// normalizeAlbumType maps MB or Spotify type labels into our canonical
// {Album, EP, Single}. MB primary-type is more reliable when present; Spotify
// uses lowercase album/single/compilation.
func normalizeAlbumType(mbType, spType string) string {
	t := strings.ToLower(firstNonEmpty(mbType, spType))
	switch t {
	case "album", "":
		return "Album"
	case "ep":
		return "EP"
	case "single":
		return "Single"
	case "compilation":
		return "Album"
	default:
		return "Album"
	}
}

// Err is a sentinel for callers that want to surface generic sync failures.
var Err = fmt.Errorf("library sync failed")
