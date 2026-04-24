// Package signal records user-behavior events and keeps derived affinity
// tables in step.
//
// Append is the single write path. For every event it:
//
//  1. inserts a row in signals (the immutable event log);
//  2. updates the subject's affinity row (artist/album/track);
//  3. propagates to the parent catalog entry — an album signal boosts its
//     artist at 0.5× weight; a track signal boosts its album at 0.5× and its
//     artist at 0.25×. Propagation is a direct affinity update, NOT an extra
//     signal row: the event log stays minimal and truthful about what the
//     user actually did.
//  4. mirrors kind-specific state into side tables (dismiss → hides,
//     heard_good/heard_bad → ratings).
//
// Everything runs in one transaction so a partial failure leaves no dangling
// affinity drift.
package signal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Kind string

const (
	LibraryAdd  Kind = "library_add"
	FollowAdd   Kind = "follow_add"
	TopRank     Kind = "top_rank"
	PlayFull    Kind = "play_full"
	PlaySkip    Kind = "play_skip"
	PlaylistAdd Kind = "playlist_add"
	HeardGood   Kind = "heard_good"
	HeardBad    Kind = "heard_bad"
	Dismiss     Kind = "dismiss"
	FilterClick Kind = "filter_click"
	OpenClick   Kind = "open_click"
)

type SubjectType string

const (
	SubjectArtist SubjectType = "artist"
	SubjectAlbum  SubjectType = "album"
	SubjectTrack  SubjectType = "track"
	SubjectLabel  SubjectType = "label"
	SubjectTag    SubjectType = "tag"
	SubjectType_  SubjectType = "type"
)

// Source tags where a signal came from, so later analysis can filter (e.g.
// "only UI-emitted bad ratings" vs "only Spotify-derived skips").
type Source string

const (
	SourceLibrary       Source = "spotify-library"
	SourceTop           Source = "spotify-top"
	SourceRecent        Source = "spotify-recent"
	SourceRecentDerived Source = "spotify-recent-derived"
	SourceUI            Source = "ui"
	SourceBackfill      Source = "backfill"
)

// Event is what callers hand to Writer.Append. Weight is optional — when
// zero, DefaultWeight for the kind is used.
type Event struct {
	UserID      string
	Kind        Kind
	SubjectType SubjectType
	SubjectID   string
	Source      Source
	Weight      float64
	Context     string // optional JSON
	Timestamp   time.Time
}

// Writer is the minimal interface services depend on. A real implementation
// lives in SQLStore; tests can stub with an in-memory slice.
type Writer interface {
	Append(ctx context.Context, e Event) error
}

// SQLStore writes to the SQLite signals table.
type SQLStore struct {
	db  *sql.DB
	now func() time.Time
}

func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db, now: time.Now}
}

// Append inserts the event, updates affinity with propagation, and writes
// side-table state. All or nothing: wrapped in a single transaction.
func (s *SQLStore) Append(ctx context.Context, e Event) error {
	if e.UserID == "" || e.Kind == "" || e.SubjectType == "" || e.SubjectID == "" {
		return errors.New("signal.Append: user_id, kind, subject_type, subject_id required")
	}
	w := e.Weight
	if w == 0 {
		w = DefaultWeight(e.Kind)
	}
	ts := e.Timestamp
	if ts.IsZero() {
		ts = s.now().UTC()
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO signals (user_id, kind, subject_type, subject_id,
		                    weight, source, context, ts)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8)
	`, e.UserID, string(e.Kind), string(e.SubjectType), e.SubjectID,
		w, string(e.Source), e.Context, ts); err != nil {
		return err
	}

	if err := applyAffinity(ctx, tx, e.UserID, e.SubjectType, e.SubjectID, w, ts); err != nil {
		return fmt.Errorf("affinity: %w", err)
	}

	if err := propagate(ctx, tx, e.UserID, e.SubjectType, e.SubjectID, w, ts); err != nil {
		return fmt.Errorf("propagate: %w", err)
	}

	if err := sideTables(ctx, tx, e.UserID, e.Kind, e.SubjectType, e.SubjectID, ts); err != nil {
		return fmt.Errorf("side: %w", err)
	}

	return tx.Commit()
}

// ── affinity updates ────────────────────────────────────────────────

// applyAffinity adds weight to the matching affinity row and bumps the
// counter + last_signal_at timestamp. Labels and tags share the same shape
// but key on label_mbid / tag_id instead of an mbid string — tag_id is an
// INTEGER so we skip it here until tags are writable (phase 6+).
func applyAffinity(ctx context.Context, tx *sql.Tx, userID string, subj SubjectType, id string, w float64, ts time.Time) error {
	var query string
	switch subj {
	case SubjectArtist:
		query = `
			INSERT INTO artist_affinity (user_id, artist_mbid, score, signal_count, last_signal_at, updated_at)
			VALUES ($1, $2, $3, 1, $4, $5)
			ON CONFLICT (user_id, artist_mbid) DO UPDATE SET
			  score          = artist_affinity.score + excluded.score,
			  signal_count   = artist_affinity.signal_count + 1,
			  last_signal_at = excluded.last_signal_at,
			  updated_at     = excluded.updated_at
		`
	case SubjectAlbum:
		query = `
			INSERT INTO album_affinity (user_id, album_mbid, score, signal_count, last_signal_at, updated_at)
			VALUES ($1, $2, $3, 1, $4, $5)
			ON CONFLICT (user_id, album_mbid) DO UPDATE SET
			  score          = album_affinity.score + excluded.score,
			  signal_count   = album_affinity.signal_count + 1,
			  last_signal_at = excluded.last_signal_at,
			  updated_at     = excluded.updated_at
		`
	case SubjectTrack:
		query = `
			INSERT INTO track_affinity (user_id, track_mbid, score, signal_count, last_signal_at, updated_at)
			VALUES ($1, $2, $3, 1, $4, $5)
			ON CONFLICT (user_id, track_mbid) DO UPDATE SET
			  score          = track_affinity.score + excluded.score,
			  signal_count   = track_affinity.signal_count + 1,
			  last_signal_at = excluded.last_signal_at,
			  updated_at     = excluded.updated_at
		`
	case SubjectLabel:
		query = `
			INSERT INTO label_affinity (user_id, label_mbid, score, signal_count, last_signal_at, updated_at)
			VALUES ($1, $2, $3, 1, $4, $5)
			ON CONFLICT (user_id, label_mbid) DO UPDATE SET
			  score          = label_affinity.score + excluded.score,
			  signal_count   = label_affinity.signal_count + 1,
			  last_signal_at = excluded.last_signal_at,
			  updated_at     = excluded.updated_at
		`
	default:
		// Tags use integer ids and aren't written anywhere yet; skip silently.
		return nil
	}
	_, err := tx.ExecContext(ctx, query, userID, id, w, ts, ts)
	return err
}

// propagate walks upward from the subject to its catalog parents and applies
// a discounted affinity boost. Lookups go through the catalog tables, so the
// parents have to already exist — library sync upserts the album before
// emitting library_add, so the chain is always valid.
func propagate(ctx context.Context, tx *sql.Tx, userID string, subj SubjectType, id string, w float64, ts time.Time) error {
	switch subj {
	case SubjectAlbum:
		artistMBID, err := lookupAlbumArtist(ctx, tx, id)
		if err != nil {
			return err
		}
		if artistMBID != "" {
			return applyAffinity(ctx, tx, userID, SubjectArtist, artistMBID, w*0.5, ts)
		}
	case SubjectTrack:
		albumMBID, artistMBID, err := lookupTrackParents(ctx, tx, id)
		if err != nil {
			return err
		}
		if albumMBID != "" {
			if err := applyAffinity(ctx, tx, userID, SubjectAlbum, albumMBID, w*0.5, ts); err != nil {
				return err
			}
			// Prefer the track's own artist_mbid; fall back to the album's
			// primary artist if tracks.artist_mbid is null.
			if artistMBID == "" {
				artistMBID, _ = lookupAlbumArtist(ctx, tx, albumMBID)
			}
		}
		if artistMBID != "" {
			return applyAffinity(ctx, tx, userID, SubjectArtist, artistMBID, w*0.25, ts)
		}
	}
	return nil
}

func lookupAlbumArtist(ctx context.Context, tx *sql.Tx, albumMBID string) (string, error) {
	var artistMBID sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT primary_artist_mbid FROM albums WHERE mbid = $1
	`, albumMBID).Scan(&artistMBID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return artistMBID.String, nil
}

func lookupTrackParents(ctx context.Context, tx *sql.Tx, trackMBID string) (album, artist string, err error) {
	var a, ar sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT album_mbid, artist_mbid FROM tracks WHERE mbid = $1
	`, trackMBID).Scan(&a, &ar)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return a.String, ar.String, err
}

// ── side tables ─────────────────────────────────────────────────────

func sideTables(ctx context.Context, tx *sql.Tx, userID string, kind Kind, subj SubjectType, id string, ts time.Time) error {
	switch kind {
	case Dismiss:
		_, err := tx.ExecContext(ctx, `
			INSERT INTO hides (user_id, subject_type, subject_id, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, subject_type, subject_id) DO UPDATE SET
			  created_at = excluded.created_at
		`, userID, string(subj), id, ts)
		return err
	case HeardGood, HeardBad:
		rating := "good"
		if kind == HeardBad {
			rating = "bad"
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO ratings (user_id, subject_type, subject_id, rating, rated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, subject_type, subject_id) DO UPDATE SET
			  rating   = excluded.rating,
			  rated_at = excluded.rated_at
		`, userID, string(subj), id, rating, ts)
		return err
	}
	return nil
}

// ── backfill / rebuild ──────────────────────────────────────────────

// Rebuild clears and repopulates all affinity tables for one user by
// aggregating the raw signals table. This is the migration path for users
// whose signals were written before Append propagated to affinity.
//
// Propagation is applied via joins on the catalog tables, so any album whose
// primary_artist_mbid couldn't be resolved at sync time simply won't
// contribute to its artist's score — the same guarantee Append gives.
func (s *SQLStore) Rebuild(ctx context.Context, userID string) error {
	if userID == "" {
		return errors.New("signal.Rebuild: user_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	for _, q := range []string{
		`DELETE FROM artist_affinity WHERE user_id = $1`,
		`DELETE FROM album_affinity  WHERE user_id = $1`,
		`DELETE FROM track_affinity  WHERE user_id = $1`,
		`DELETE FROM label_affinity  WHERE user_id = $1`,
	} {
		if _, err := tx.ExecContext(ctx, q, userID); err != nil {
			return err
		}
	}

	// Direct subject→affinity aggregations.
	direct := []struct{ sel, ins, key string }{
		{
			sel: `SELECT subject_id, SUM(weight), COUNT(*), MAX(ts) FROM signals
			      WHERE user_id = $1 AND subject_type = 'artist' GROUP BY subject_id`,
			ins: `INSERT INTO artist_affinity (user_id, artist_mbid, score, signal_count, last_signal_at, updated_at)
			      VALUES ($1, $2, $3, $4, $5, $6)
			      ON CONFLICT (user_id, artist_mbid) DO UPDATE SET
			        score = artist_affinity.score + excluded.score,
			        signal_count = artist_affinity.signal_count + excluded.signal_count,
			        last_signal_at = GREATEST(artist_affinity.last_signal_at, excluded.last_signal_at),
			        updated_at = excluded.updated_at`,
		},
		{
			sel: `SELECT subject_id, SUM(weight), COUNT(*), MAX(ts) FROM signals
			      WHERE user_id = $1 AND subject_type = 'album' GROUP BY subject_id`,
			ins: `INSERT INTO album_affinity (user_id, album_mbid, score, signal_count, last_signal_at, updated_at)
			      VALUES ($1, $2, $3, $4, $5, $6)
			      ON CONFLICT (user_id, album_mbid) DO UPDATE SET
			        score = album_affinity.score + excluded.score,
			        signal_count = album_affinity.signal_count + excluded.signal_count,
			        last_signal_at = GREATEST(album_affinity.last_signal_at, excluded.last_signal_at),
			        updated_at = excluded.updated_at`,
		},
		{
			sel: `SELECT subject_id, SUM(weight), COUNT(*), MAX(ts) FROM signals
			      WHERE user_id = $1 AND subject_type = 'track' GROUP BY subject_id`,
			ins: `INSERT INTO track_affinity (user_id, track_mbid, score, signal_count, last_signal_at, updated_at)
			      VALUES ($1, $2, $3, $4, $5, $6)
			      ON CONFLICT (user_id, track_mbid) DO UPDATE SET
			        score = track_affinity.score + excluded.score,
			        signal_count = track_affinity.signal_count + excluded.signal_count,
			        last_signal_at = GREATEST(track_affinity.last_signal_at, excluded.last_signal_at),
			        updated_at = excluded.updated_at`,
		},
		{
			sel: `SELECT subject_id, SUM(weight), COUNT(*), MAX(ts) FROM signals
			      WHERE user_id = $1 AND subject_type = 'label' GROUP BY subject_id`,
			ins: `INSERT INTO label_affinity (user_id, label_mbid, score, signal_count, last_signal_at, updated_at)
			      VALUES ($1, $2, $3, $4, $5, $6)
			      ON CONFLICT (user_id, label_mbid) DO UPDATE SET
			        score = label_affinity.score + excluded.score,
			        signal_count = label_affinity.signal_count + excluded.signal_count,
			        last_signal_at = GREATEST(label_affinity.last_signal_at, excluded.last_signal_at),
			        updated_at = excluded.updated_at`,
		},
	}
	now := s.now().UTC()
	for i, d := range direct {
		if err := replayAggregated(ctx, tx, d.sel, d.ins, userID, now); err != nil {
			return fmt.Errorf("replay#%d: %w", i, err)
		}
	}

	// Propagation: album signals → artist at 0.5×, track signals → album at
	// 0.5×, track signals → artist at 0.25× (via the track's album).
	propAlbum := `
		INSERT INTO artist_affinity (user_id, artist_mbid, score, signal_count, last_signal_at, updated_at)
		SELECT s.user_id, a.primary_artist_mbid, SUM(s.weight) * 0.5, COUNT(*), MAX(s.ts), $1
		FROM signals s
		JOIN albums a ON a.mbid = s.subject_id
		WHERE s.user_id = $2 AND s.subject_type = 'album'
		  AND a.primary_artist_mbid IS NOT NULL
		GROUP BY s.user_id, a.primary_artist_mbid
		ON CONFLICT (user_id, artist_mbid) DO UPDATE SET
		  score = artist_affinity.score + excluded.score,
		  signal_count = artist_affinity.signal_count + excluded.signal_count,
		  last_signal_at = GREATEST(artist_affinity.last_signal_at, excluded.last_signal_at),
		  updated_at = excluded.updated_at`
	if _, err := tx.ExecContext(ctx, propAlbum, now, userID); err != nil {
		return fmt.Errorf("propAlbum: %w", err)
	}

	propTrackAlbum := `
		INSERT INTO album_affinity (user_id, album_mbid, score, signal_count, last_signal_at, updated_at)
		SELECT s.user_id, t.album_mbid, SUM(s.weight) * 0.5, COUNT(*), MAX(s.ts), $1
		FROM signals s
		JOIN tracks t ON t.mbid = s.subject_id
		WHERE s.user_id = $2 AND s.subject_type = 'track'
		  AND t.album_mbid IS NOT NULL
		GROUP BY s.user_id, t.album_mbid
		ON CONFLICT (user_id, album_mbid) DO UPDATE SET
		  score = album_affinity.score + excluded.score,
		  signal_count = album_affinity.signal_count + excluded.signal_count,
		  last_signal_at = GREATEST(album_affinity.last_signal_at, excluded.last_signal_at),
		  updated_at = excluded.updated_at`
	if _, err := tx.ExecContext(ctx, propTrackAlbum, now, userID); err != nil {
		return fmt.Errorf("propTrackAlbum: %w", err)
	}

	propTrackArtist := `
		INSERT INTO artist_affinity (user_id, artist_mbid, score, signal_count, last_signal_at, updated_at)
		SELECT s.user_id, COALESCE(t.artist_mbid, a.primary_artist_mbid),
		       SUM(s.weight) * 0.25, COUNT(*), MAX(s.ts), $1
		FROM signals s
		JOIN tracks t ON t.mbid = s.subject_id
		LEFT JOIN albums a ON a.mbid = t.album_mbid
		WHERE s.user_id = $2 AND s.subject_type = 'track'
		  AND COALESCE(t.artist_mbid, a.primary_artist_mbid) IS NOT NULL
		GROUP BY s.user_id, COALESCE(t.artist_mbid, a.primary_artist_mbid)
		ON CONFLICT (user_id, artist_mbid) DO UPDATE SET
		  score = artist_affinity.score + excluded.score,
		  signal_count = artist_affinity.signal_count + excluded.signal_count,
		  last_signal_at = GREATEST(artist_affinity.last_signal_at, excluded.last_signal_at),
		  updated_at = excluded.updated_at`
	if _, err := tx.ExecContext(ctx, propTrackArtist, now, userID); err != nil {
		return fmt.Errorf("propTrackArtist: %w", err)
	}

	return tx.Commit()
}

// replayAggregated drives rows from a SELECT ... GROUP BY into an INSERT ...
// ON CONFLICT upsert. Using two steps keeps the dialect portable.
func replayAggregated(ctx context.Context, tx *sql.Tx, sel, ins, userID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, sel, userID)
	if err != nil {
		return fmt.Errorf("sel: %w", err)
	}
	defer rows.Close()
	type row struct {
		subjectID string
		score     float64
		count     int
		lastTS    sql.NullTime
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.subjectID, &r.score, &r.count, &r.lastTS); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	rows.Close()
	for _, r := range batch {
		if _, err := tx.ExecContext(ctx, ins, userID, r.subjectID, r.score, r.count, r.lastTS, now); err != nil {
			return fmt.Errorf("ins: %w", err)
		}
	}
	return nil
}

// ── default weights ─────────────────────────────────────────────────

// DefaultWeight returns the baseline weight per kind. Calibrated in spec
// section 4.4; tunable later without touching call sites.
func DefaultWeight(k Kind) float64 {
	switch k {
	case LibraryAdd:
		return 1.0
	case FollowAdd:
		return 1.2
	case TopRank:
		return 2.0
	case PlayFull:
		return 0.3
	case PlaySkip:
		return -0.3
	case PlaylistAdd:
		return 0.5
	case HeardGood:
		return 1.5
	case HeardBad:
		return -1.5
	case Dismiss:
		return -0.5
	case FilterClick:
		return 0.05
	case OpenClick:
		return 0.1
	default:
		return 0
	}
}
