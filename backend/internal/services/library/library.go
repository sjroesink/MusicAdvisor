// Package library performs a prioritized Spotify library sync for one user.
//
// The sync runs in three phases:
//
//  1. Gather — fetch followed artists and saved albums from Spotify into
//     memory. Spotify calls are cheap; no MusicBrainz traffic yet.
//     Individual liked tracks are intentionally excluded: they're noisier
//     than curated albums/follows and would multiply MB calls by 10×. We
//     can add them back in a later phase if the signal turns out to be
//     useful.
//
//  2. Rank — bucket every item by its primary artist and score the bucket:
//     albums * 3 + (followed ? 10 : 0). Sort buckets descending. The user's
//     most-relevant artists end up at the top, so their catalog is resolved
//     first.
//
//  3. Resolve — walk the ranked buckets. For each: resolve the artist via
//     MusicBrainz, upsert the catalog row, emit follow_add if followed,
//     then resolve and upsert that artist's albums, emitting library_add
//     signals as we go.
//
// Why this order: MusicBrainz caps all clients at 1 req/s. Priority order
// means the first minutes of a sync already cover the user's heaviest-
// listened artists, which is what downstream discover/new-release jobs care
// about most. Less-frequent artists resolve later without blocking value.
package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/resolver"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
)

// TokenProvider decouples the sync from the user service.
type TokenProvider interface {
	AccessToken(ctx context.Context, userID, provider string,
		refresh func(ctx context.Context, externalID, refreshToken string) (string, string, time.Time, error),
	) (string, error)
}

// SpotifyClient is the slice of *spotify.Client we need for the current
// sync. FetchSavedTracks lives on *spotify.Client but isn't wired here; a
// later phase can extend this interface when we decide to pull tracks.
type SpotifyClient interface {
	FetchSavedAlbums(ctx context.Context, accessToken string, onAlbum func(spotify.SavedAlbum) error) (int, error)
	FetchFollowedArtists(ctx context.Context, accessToken string, onArtist func(spotify.FollowedArtist) error) (int, error)
	RefreshToken(ctx context.Context, refreshToken string) (spotify.TokenSet, error)
}

// Resolver is the minimal surface we need. ResolveTrack lives on the
// resolver service but isn't used here.
type Resolver interface {
	ResolveAlbum(ctx context.Context, spotifyID, upc string) (resolver.Result, error)
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
		db:       db,
		tokens:   tokens,
		spotify:  sp,
		resolver: res,
		signals:  sigs,
		logger:   logger,
		now:      time.Now,
	}
}

// RunResult summarizes one sync run. TracksAdded is retained for forward
// compatibility; this phase of the sync doesn't populate it.
type RunResult struct {
	RunID        int64
	Status       string // "ok" | "partial" | "failed"
	AlbumsAdded  int
	TracksAdded  int
	ArtistsAdded int
	Unresolved   int
	Errors       int
	DurationMs   int64
}

// ── Gather-phase aggregates ─────────────────────────────────────────

// artistBucket groups a user's library entries by their primary artist.
// An artist can appear as a follow, as the primary artist of a saved album,
// or both. score is computed once in rank().
type artistBucket struct {
	SpotifyID string
	Name      string

	Followed bool
	Albums   []spotify.SavedAlbum

	score float64
}

// Sync fetches all Spotify library data, buckets by artist, ranks, and
// resolves against MusicBrainz in that priority order.
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
				// Only terminal failures should lock the account; the user
				// service uses the AsTerminal sentinel to decide.
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

	s.logger.Info("sync started",
		"user_id", userID,
		"run_id", runID,
	)

	gatherStart := s.now()
	buckets, gatherErr := s.gather(ctx, accessToken)
	s.rank(buckets)
	ranked := bucketsByScoreDesc(buckets)

	totalAlbums := 0
	for _, b := range ranked {
		totalAlbums += len(b.Albums)
	}
	s.logger.Info("sync gathered library",
		"user_id", userID,
		"distinct_artists", len(ranked),
		"total_albums", totalAlbums,
		"gather_ms", s.now().Sub(gatherStart).Milliseconds(),
		"gather_error", errText(gatherErr),
	)

	for i, b := range ranked {
		bucketStart := s.now()
		before := result
		s.logger.Info("resolving bucket",
			"user_id", userID,
			"rank", i+1,
			"total", len(ranked),
			"artist", b.Name,
			"score", b.score,
			"albums", len(b.Albums),
			"followed", b.Followed,
		)

		s.resolveBucket(ctx, userID, b, &result)

		s.logger.Info("bucket resolved",
			"user_id", userID,
			"rank", i+1,
			"artist", b.Name,
			"artist_added", result.ArtistsAdded-before.ArtistsAdded,
			"albums_added", result.AlbumsAdded-before.AlbumsAdded,
			"unresolved", result.Unresolved-before.Unresolved,
			"errors", result.Errors-before.Errors,
			"duration_ms", s.now().Sub(bucketStart).Milliseconds(),
		)
	}

	status := "ok"
	var finalErr string
	if gatherErr != nil || result.Errors > 0 || result.Unresolved > 0 {
		status = "partial"
	}
	if gatherErr != nil {
		finalErr = appendErr(finalErr, "gather: "+gatherErr.Error())
	}

	finished := s.now().UTC()
	itemsAdded := result.AlbumsAdded + result.TracksAdded + result.ArtistsAdded
	s.finishRun(ctx, runID, status, itemsAdded, finalErr, finished)
	result.Status = status
	result.DurationMs = finished.Sub(started).Milliseconds()
	return result, nil
}

// ── Phase 1: gather ─────────────────────────────────────────────────

func (s *Service) gather(ctx context.Context, accessToken string) (map[string]*artistBucket, error) {
	buckets := map[string]*artistBucket{}
	get := func(id, name string) *artistBucket {
		if id == "" {
			return nil
		}
		b, ok := buckets[id]
		if !ok {
			b = &artistBucket{SpotifyID: id, Name: name}
			buckets[id] = b
		}
		if b.Name == "" && name != "" {
			b.Name = name
		}
		return b
	}

	var combined error

	_, err := s.spotify.FetchFollowedArtists(ctx, accessToken, func(a spotify.FollowedArtist) error {
		if b := get(a.SpotifyID, a.Name); b != nil {
			b.Followed = true
		}
		return nil
	})
	if err != nil {
		combined = appendErrErr(combined, fmt.Errorf("followed: %w", err))
	}

	_, err = s.spotify.FetchSavedAlbums(ctx, accessToken, func(a spotify.SavedAlbum) error {
		if b := get(a.ArtistID, a.ArtistName); b != nil {
			b.Albums = append(b.Albums, a)
		}
		return nil
	})
	if err != nil {
		combined = appendErrErr(combined, fmt.Errorf("albums: %w", err))
	}

	return buckets, combined
}

// ── Phase 2: rank ───────────────────────────────────────────────────

func (s *Service) rank(buckets map[string]*artistBucket) {
	for _, b := range buckets {
		b.score = scoreBucket(b)
	}
}

// scoreBucket ranks an artist. An explicit follow is the strongest signal;
// a saved album is a deliberate act per album.
func scoreBucket(b *artistBucket) float64 {
	score := float64(len(b.Albums)) * 3.0
	if b.Followed {
		score += 10
	}
	return score
}

func bucketsByScoreDesc(buckets map[string]*artistBucket) []*artistBucket {
	out := make([]*artistBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		// Stable tie-breaker: followed before not-followed; then alpha by name.
		if out[i].Followed != out[j].Followed {
			return out[i].Followed
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ── Phase 3: resolve one bucket ─────────────────────────────────────

func (s *Service) resolveBucket(ctx context.Context, userID string, b *artistBucket, r *RunResult) {
	// Try to resolve the artist. For followed artists we need an MBID even
	// if MB can't match the name (we still want a saved_artists row). For
	// non-followed buckets we skip the placeholder so album/track resolution
	// can freely upsert the real-MBID artist row — creating the placeholder
	// eagerly would otherwise collide on the (spotify_id) UNIQUE constraint.
	var artistMBID string
	ares, err := s.resolver.ResolveArtistByName(ctx, b.SpotifyID, b.Name)
	switch {
	case errors.Is(err, resolver.ErrUnresolved):
		if b.Followed {
			if b.Name != "" && b.SpotifyID != "" {
				artistMBID = "sp:" + b.SpotifyID
				if upErr := s.upsertArtistPlaceholder(ctx, artistMBID, b.SpotifyID, b.Name); upErr != nil {
					r.Errors++
					return
				}
			}
			r.Unresolved++
		}
		// non-followed + unresolved: silently skip; a later album/track
		// resolution may still land the real artist row.
	case err != nil:
		s.logger.Warn("resolve artist failed", "spotify_id", b.SpotifyID, "err", err)
		r.Errors++
		return
	default:
		artistMBID = ares.MBID
		if err := s.upsertArtist(ctx, artistMBID, b.SpotifyID, b.Name); err != nil {
			r.Errors++
			return
		}
		s.logger.Debug("resolved artist",
			"spotify_id", b.SpotifyID,
			"mbid", artistMBID,
			"name", b.Name,
			"confidence", ares.Confidence,
		)
	}

	// Followed artists get a saved_artists row + follow_add signal. The
	// signal fires only on first save: a resync of an already-followed
	// artist shouldn't keep re-counting affinity.
	if b.Followed && artistMBID != "" {
		inserted, err := s.insertSavedArtist(ctx, userID, artistMBID)
		if err != nil {
			r.Errors++
		} else if inserted {
			if err := s.signals.Append(ctx, signal.Event{
				UserID:      userID,
				Kind:        signal.FollowAdd,
				SubjectType: signal.SubjectArtist,
				SubjectID:   artistMBID,
				Source:      signal.SourceLibrary,
			}); err != nil {
				r.Errors++
			} else {
				r.ArtistsAdded++
			}
		}
	}

	for _, a := range b.Albums {
		s.resolveOneAlbum(ctx, userID, a, artistMBID, r)
	}
}

func (s *Service) resolveOneAlbum(ctx context.Context, userID string, a spotify.SavedAlbum, fallbackArtistMBID string, r *RunResult) {
	res, err := s.resolver.ResolveAlbum(ctx, a.SpotifyID, a.UPC)
	if errors.Is(err, resolver.ErrUnresolved) {
		r.Unresolved++
		return
	}
	if err != nil {
		s.logger.Warn("album resolve failed", "spotify_id", a.SpotifyID, "err", err)
		r.Errors++
		return
	}

	artistMBID := res.ArtistMBID
	if artistMBID == "" {
		artistMBID = fallbackArtistMBID
	}
	if artistMBID != "" && a.ArtistID != "" && res.ArtistName != "" {
		_ = s.upsertArtist(ctx, artistMBID, a.ArtistID, res.ArtistName)
	}

	if err := s.upsertAlbum(ctx, res, a, artistMBID); err != nil {
		r.Errors++
		return
	}
	inserted, err := s.insertSavedAlbum(ctx, userID, res.MBID, a.AddedAt)
	if err != nil {
		r.Errors++
		return
	}
	if !inserted {
		// Already saved on a prior sync — counter stays at whatever it is.
		return
	}
	if err := s.signals.Append(ctx, signal.Event{
		UserID:      userID,
		Kind:        signal.LibraryAdd,
		SubjectType: signal.SubjectAlbum,
		SubjectID:   res.MBID,
		Source:      signal.SourceLibrary,
	}); err != nil {
		r.Errors++
		return
	}
	r.AlbumsAdded++
	s.logger.Debug("resolved album",
		"spotify_id", a.SpotifyID,
		"mbid", res.MBID,
		"title", firstNonEmpty(res.Title, a.Name),
		"type", normalizeAlbumType(res.PrimaryType, a.AlbumType),
	)
}

// ── catalog upserts (unchanged) ─────────────────────────────────────

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

// ── saved_* ─────────────────────────────────────────────────────────

// insertSavedArtist returns true when the row was actually inserted. Callers
// should skip emitting the follow_add signal when it wasn't — a resync of an
// already-saved artist shouldn't keep boosting affinity forever.
func (s *Service) insertSavedArtist(ctx context.Context, userID, artistMBID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO saved_artists (user_id, artist_mbid, saved_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, artistMBID, s.now().UTC())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Service) insertSavedAlbum(ctx context.Context, userID, albumMBID string, savedAt time.Time) (bool, error) {
	if savedAt.IsZero() {
		savedAt = s.now().UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO saved_albums (user_id, album_mbid, saved_at)
		VALUES (?, ?, ?)
		ON CONFLICT DO NOTHING
	`, userID, albumMBID, savedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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

func appendErrErr(existing, add error) error {
	if existing == nil {
		return add
	}
	return fmt.Errorf("%s; %s", existing.Error(), add.Error())
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

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
