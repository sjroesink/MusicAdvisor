// Package resolver maps Spotify IDs to MusicBrainz IDs. Results are cached
// in SQLite (resolver_cache); unresolved lookups are tombstoned so we don't
// hammer MusicBrainz on every sync for items that simply aren't mapped.
//
// MusicBrainz imposes a 1 req/s global rate limit, so resolving a full
// library is intentionally slow. Callers should structure sync loops around
// that (process in background, emit partial progress, record sync_runs).
package resolver

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
)

// SubjectType mirrors the enum used in resolver_cache.subject_type.
type SubjectType string

const (
	SubjectTrack  SubjectType = "track"
	SubjectAlbum  SubjectType = "album"
	SubjectArtist SubjectType = "artist"
)

// Result is what a successful resolve returns. Unresolved lookups return
// ErrUnresolved and cache a tombstone.
type Result struct {
	MBID       string
	Confidence float64

	// Side-channel identity info discovered during the lookup. The sync
	// orchestrator uses these to populate catalog tables without issuing
	// extra MB calls (a lookup of a track returns the parent album + artist;
	// a lookup of an album returns the artist; etc.).
	ArtistMBID     string
	ArtistName     string
	ReleaseGroupID string
	Title          string
	PrimaryType    string // album lookups only
	FirstReleaseDate string
}

var (
	ErrUnresolved = errors.New("resolver: no MBID mapping for subject")
)

// Service resolves Spotify IDs to MBIDs. Safe for concurrent use; actual MB
// serialization happens inside the embedded MusicBrainz client.
type Service struct {
	db      *sql.DB
	mb      MusicBrainz
	now     func() time.Time
	ttl     time.Duration
	missTTL time.Duration
}

// MusicBrainz is the minimal surface needed; declaring it here lets tests
// inject a fake client.
type MusicBrainz interface {
	LookupTrackByISRC(ctx context.Context, isrc string) (musicbrainz.Track, error)
	LookupAlbumByUPC(ctx context.Context, upc string) (musicbrainz.Album, error)
	SearchArtistByName(ctx context.Context, name string) (musicbrainz.Artist, error)
}

type Option func(*Service)

// WithTTL overrides the default 90-day hit TTL.
func WithTTL(d time.Duration) Option { return func(s *Service) { s.ttl = d } }

// WithMissTTL overrides the default 30-day tombstone TTL.
func WithMissTTL(d time.Duration) Option { return func(s *Service) { s.missTTL = d } }

func New(db *sql.DB, mb MusicBrainz, opts ...Option) *Service {
	s := &Service{
		db:      db,
		mb:      mb,
		now:     time.Now,
		ttl:     90 * 24 * time.Hour,
		missTTL: 30 * 24 * time.Hour,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ResolveTrack maps (spotifyTrackID, isrc) → MBID. ISRC is what Spotify
// returns in /tracks/{id}.external_ids; callers pass it through.
func (s *Service) ResolveTrack(ctx context.Context, spotifyID, isrc string) (Result, error) {
	if cached, ok, err := s.checkCache(ctx, spotifyID, SubjectTrack); err != nil {
		return Result{}, err
	} else if ok {
		if cached.MBID == "" {
			return Result{}, ErrUnresolved
		}
		return cached, nil
	}
	if isrc == "" {
		s.writeTombstone(ctx, spotifyID, SubjectTrack)
		return Result{}, ErrUnresolved
	}
	mb, err := s.mb.LookupTrackByISRC(ctx, isrc)
	if err != nil {
		if errors.Is(err, musicbrainz.ErrNotFound) {
			s.writeTombstone(ctx, spotifyID, SubjectTrack)
			return Result{}, ErrUnresolved
		}
		return Result{}, err
	}
	res := Result{
		MBID:           mb.MBID,
		Confidence:     0.95,
		ArtistMBID:     mb.ArtistID,
		ArtistName:     mb.ArtistName,
		ReleaseGroupID: mb.ReleaseGroupID,
		Title:          mb.Title,
	}
	_ = s.writeHit(ctx, spotifyID, SubjectTrack, res)
	return res, nil
}

// ResolveAlbum maps (spotifyAlbumID, upc) → MBID release-group.
func (s *Service) ResolveAlbum(ctx context.Context, spotifyID, upc string) (Result, error) {
	if cached, ok, err := s.checkCache(ctx, spotifyID, SubjectAlbum); err != nil {
		return Result{}, err
	} else if ok {
		if cached.MBID == "" {
			return Result{}, ErrUnresolved
		}
		return cached, nil
	}
	if upc == "" {
		s.writeTombstone(ctx, spotifyID, SubjectAlbum)
		return Result{}, ErrUnresolved
	}
	mb, err := s.mb.LookupAlbumByUPC(ctx, upc)
	if err != nil {
		if errors.Is(err, musicbrainz.ErrNotFound) {
			s.writeTombstone(ctx, spotifyID, SubjectAlbum)
			return Result{}, ErrUnresolved
		}
		return Result{}, err
	}
	res := Result{
		MBID:             mb.MBID,
		Confidence:       0.85,
		ArtistMBID:       mb.ArtistID,
		ArtistName:       mb.ArtistName,
		Title:            mb.Title,
		PrimaryType:      mb.PrimaryType,
		FirstReleaseDate: mb.FirstReleaseDate,
	}
	_ = s.writeHit(ctx, spotifyID, SubjectAlbum, res)
	return res, nil
}

// ResolveArtistByName maps (spotifyArtistID, name) → MBID. Requires score
// ≥ 90 from MB; below that we tombstone as ambiguous.
func (s *Service) ResolveArtistByName(ctx context.Context, spotifyID, name string) (Result, error) {
	if cached, ok, err := s.checkCache(ctx, spotifyID, SubjectArtist); err != nil {
		return Result{}, err
	} else if ok {
		if cached.MBID == "" {
			return Result{}, ErrUnresolved
		}
		return cached, nil
	}
	if name == "" {
		s.writeTombstone(ctx, spotifyID, SubjectArtist)
		return Result{}, ErrUnresolved
	}
	mb, err := s.mb.SearchArtistByName(ctx, name)
	if err != nil {
		if errors.Is(err, musicbrainz.ErrNotFound) {
			s.writeTombstone(ctx, spotifyID, SubjectArtist)
			return Result{}, ErrUnresolved
		}
		return Result{}, err
	}
	if mb.Score < 90 {
		s.writeTombstone(ctx, spotifyID, SubjectArtist)
		return Result{}, ErrUnresolved
	}
	res := Result{
		MBID:       mb.MBID,
		Confidence: float64(mb.Score) / 100.0,
		ArtistMBID: mb.MBID,
		ArtistName: mb.Name,
	}
	_ = s.writeHit(ctx, spotifyID, SubjectArtist, res)
	return res, nil
}

func (s *Service) checkCache(ctx context.Context, spotifyID string, st SubjectType) (Result, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT mbid, confidence, resolved_at
		FROM resolver_cache
		WHERE spotify_id = $1 AND subject_type = $2
	`, spotifyID, string(st))

	var (
		mbid       sql.NullString
		confidence sql.NullFloat64
		resolvedAt time.Time
	)
	err := row.Scan(&mbid, &confidence, &resolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	age := s.now().Sub(resolvedAt)
	if !mbid.Valid {
		if age > s.missTTL {
			return Result{}, false, nil
		}
		return Result{}, true, nil
	}
	if age > s.ttl {
		return Result{}, false, nil
	}
	return Result{MBID: mbid.String, Confidence: confidence.Float64}, true, nil
}

func (s *Service) writeHit(ctx context.Context, spotifyID string, st SubjectType, res Result) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO resolver_cache (spotify_id, subject_type, mbid, confidence, resolved_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (spotify_id, subject_type) DO UPDATE SET
			mbid = excluded.mbid,
			confidence = excluded.confidence,
			resolved_at = excluded.resolved_at
	`, spotifyID, string(st), res.MBID, res.Confidence, s.now().UTC())
	return err
}

func (s *Service) writeTombstone(ctx context.Context, spotifyID string, st SubjectType) {
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO resolver_cache (spotify_id, subject_type, mbid, confidence, resolved_at)
		VALUES ($1, $2, NULL, NULL, $3)
		ON CONFLICT (spotify_id, subject_type) DO UPDATE SET
			mbid = NULL, confidence = NULL, resolved_at = excluded.resolved_at
	`, spotifyID, string(st), s.now().UTC())
}
