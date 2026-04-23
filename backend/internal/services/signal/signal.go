// Package signal appends user-behavior events to the signals table.
//
// Phase 3 writes library_add and follow_add signals during library sync.
// Phase 4 will extend this with UI-emitted signals (heard_good/bad, dismiss,
// filter_click, open_click), signal propagation (album→artist boost), and
// incremental affinity updates. The append-only core stays the same.
package signal

import (
	"context"
	"database/sql"
	"errors"
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
	SourceLibrary    Source = "spotify-library"
	SourceTop        Source = "spotify-top"
	SourceRecent     Source = "spotify-recent"
	SourceRecentDerived Source = "spotify-recent-derived"
	SourceUI         Source = "ui"
	SourceBackfill   Source = "backfill"
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

// Append inserts one row. Empty Weight is replaced by DefaultWeight(kind).
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
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO signals (user_id, kind, subject_type, subject_id,
		                    weight, source, context, ts)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)
	`, e.UserID, string(e.Kind), string(e.SubjectType), e.SubjectID,
		w, string(e.Source), e.Context, ts)
	return err
}

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
