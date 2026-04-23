package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
)

type FeedDeps struct {
	DB     *sql.DB
	Logger *slog.Logger
}

// FeedResponse is the flat shape the frontend reads on mount. All fields
// are safe to render as-is; the server pre-formats dates and human-readable
// reasons so the UI has no per-row transformation work.
type FeedResponse struct {
	Header      FeedHeader      `json:"header"`
	NewReleases []FeedCard      `json:"new_releases"`
	Discover    []FeedCard      `json:"discover"`
	Ratings     []FeedRating    `json:"ratings"`
	Hides       []FeedHide      `json:"hides"`
}

type FeedHeader struct {
	LibraryCount int       `json:"library_count"`
	LastSyncAt   time.Time `json:"last_sync_at,omitempty"`
	Status       string    `json:"status"` // "idle" | "syncing" | "ready" | "error"
}

type FeedCard struct {
	ID          string  `json:"id"`
	SubjectType string  `json:"subject_type"`
	Artist      string  `json:"artist"`
	Title       string  `json:"title"`
	Year        int     `json:"year,omitempty"`
	Date        string  `json:"date,omitempty"`
	Type        string  `json:"type"`
	Tracks      int     `json:"tracks,omitempty"`
	Length      string  `json:"length,omitempty"`
	Reason      string  `json:"reason"`
	Cover       string  `json:"cover,omitempty"`
	Score       float64 `json:"score"`
	Source      string  `json:"source"`
}

type FeedRating struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Rating      string `json:"rating"`
}

type FeedHide struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
}

// Feed reads everything the frontend needs to render the initial screen.
// All queries are small and keyed on user_id, so a single request fits
// well within the 30s router timeout even on modest libraries.
func Feed(d FeedDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := auth.UserIDFromContext(r.Context())
		if userID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}

		resp := FeedResponse{}

		header, err := readFeedHeader(r.Context(), d.DB, userID)
		if err != nil {
			d.Logger.Error("feed: header", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not load feed")
			return
		}
		resp.Header = header

		newReleases, err := readCards(r.Context(), d.DB, userID, "mb_new_release")
		if err != nil {
			d.Logger.Error("feed: new releases", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not load feed")
			return
		}
		resp.NewReleases = newReleases

		discover, err := readCards(r.Context(), d.DB, userID, "listenbrainz")
		if err != nil {
			d.Logger.Error("feed: discover", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not load feed")
			return
		}
		resp.Discover = discover

		ratings, err := readRatings(r.Context(), d.DB, userID)
		if err != nil {
			d.Logger.Error("feed: ratings", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not load feed")
			return
		}
		resp.Ratings = ratings

		hides, err := readHides(r.Context(), d.DB, userID)
		if err != nil {
			d.Logger.Error("feed: hides", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not load feed")
			return
		}
		resp.Hides = hides

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// ── header ──────────────────────────────────────────────────────────

func readFeedHeader(ctx context.Context, db *sql.DB, userID string) (FeedHeader, error) {
	var h FeedHeader

	// Library count = saved_artists + saved_albums (tracks not synced in MVP).
	var artistCount, albumCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM saved_artists WHERE user_id = ?`, userID).Scan(&artistCount); err != nil {
		return h, err
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM saved_albums WHERE user_id = ?`, userID).Scan(&albumCount); err != nil {
		return h, err
	}
	h.LibraryCount = artistCount + albumCount

	// Latest sync_runs row drives status + last_sync_at.
	var started sql.NullString
	var status sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT started_at, status
		FROM sync_runs
		WHERE user_id = ?
		ORDER BY started_at DESC
		LIMIT 1
	`, userID).Scan(&started, &status)
	if err != nil && err != sql.ErrNoRows {
		return h, err
	}
	h.Status = "idle"
	if status.Valid {
		switch status.String {
		case "running":
			h.Status = "syncing"
		case "failed":
			h.Status = "error"
		default:
			h.Status = "ready"
		}
		if started.Valid {
			if t, err := parseSQLiteTime(started.String); err == nil {
				h.LastSyncAt = t
			}
		}
	}
	return h, nil
}

// ── cards ───────────────────────────────────────────────────────────

// readCards walks discover_candidates for one source, joins catalog tables,
// and formats each row into a FeedCard. Expired candidates are excluded.
// The "now" cutoff is explicit rather than SQLite's CURRENT_TIMESTAMP —
// modernc's driver stores time.Time with a timezone suffix that doesn't
// lexicographically compare cleanly against CURRENT_TIMESTAMP's
// zoneless "YYYY-MM-DD HH:MM:SS".
func readCards(ctx context.Context, db *sql.DB, userID, source string) ([]FeedCard, error) {
	now := time.Now().UTC()
	rows, err := db.QueryContext(ctx, `
		SELECT dc.subject_type, dc.subject_id, dc.raw_score, dc.reason_data,
		       COALESCE(a.title, '')        AS title,
		       COALESCE(a.release_date, '') AS release_date,
		       COALESCE(a.type, 'Album')    AS type,
		       COALESCE(a.track_count, 0)   AS tracks,
		       COALESCE(a.length_sec, 0)    AS length_sec,
		       COALESCE(ar.name, '')        AS artist_name
		FROM discover_candidates dc
		LEFT JOIN albums  a  ON a.mbid  = dc.subject_id AND dc.subject_type = 'album'
		LEFT JOIN artists ar ON ar.mbid = a.primary_artist_mbid
		WHERE dc.user_id = ?
		  AND dc.source  = ?
		  AND (dc.expires_at IS NULL OR unixepoch(dc.expires_at) > unixepoch(?))
		ORDER BY dc.raw_score DESC
		LIMIT 50
	`, userID, source, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FeedCard
	for rows.Next() {
		var (
			subjectType string
			subjectID   string
			score       float64
			reasonRaw   string
			title       string
			releaseDate string
			albumType   string
			trackCount  int
			lengthSec   int
			artistName  string
		)
		if err := rows.Scan(&subjectType, &subjectID, &score, &reasonRaw,
			&title, &releaseDate, &albumType, &trackCount, &lengthSec, &artistName); err != nil {
			return nil, err
		}
		c := FeedCard{
			ID:          subjectID,
			SubjectType: subjectType,
			Artist:      artistName,
			Title:       title,
			Type:        albumType,
			Tracks:      trackCount,
			Score:       score,
			Source:      source,
		}
		if y, d := splitReleaseDate(releaseDate); y > 0 {
			c.Year = y
			c.Date = d
		}
		if lengthSec > 0 {
			c.Length = fmt.Sprintf("%d min", (lengthSec+59)/60)
		}
		c.Reason = formatReason(source, reasonRaw, artistName)
		c.Cover = coverFrom(artistName, title)
		out = append(out, c)
	}
	return out, rows.Err()
}

// splitReleaseDate turns "2026-04-18" into (2026, "Apr 18"). Accepts the
// partial MB formats "YYYY-MM" and "YYYY" too.
func splitReleaseDate(s string) (year int, date string) {
	if s == "" {
		return 0, ""
	}
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			y := t.Year()
			d := ""
			if layout == "2006-01-02" {
				d = t.Format("Jan 02")
			} else if layout == "2006-01" {
				d = t.Format("Jan")
			}
			return y, d
		}
	}
	return 0, ""
}

func coverFrom(artist, title string) string {
	initials := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return "??"
		}
		parts := strings.Fields(s)
		var out []byte
		for _, p := range parts {
			out = append(out, strings.ToUpper(p[:1])...)
			if len(out) >= 2 {
				break
			}
		}
		if len(out) == 0 {
			return "??"
		}
		return string(out)
	}
	return initials(artist) + " · " + strings.ToUpper(title)
}

// formatReason produces a short human explanation from the candidate's
// reason_data JSON. Falls back to a generic phrasing when parsing fails.
func formatReason(source, raw, artist string) string {
	type reason struct {
		ViaArtistName string  `json:"via_artist_name"`
		ReleaseDate   string  `json:"release_date"`
		PrimaryType   string  `json:"primary_type"`
		LBScore       float64 `json:"lb_score"`
	}
	var r reason
	_ = json.Unmarshal([]byte(raw), &r)

	switch source {
	case "mb_new_release":
		if r.ViaArtistName != "" {
			return "New " + strings.ToLower(or(r.PrimaryType, "release")) + " from " + r.ViaArtistName + "."
		}
		return "New release from an artist you've saved."
	case "listenbrainz":
		if r.ViaArtistName != "" {
			return "Similar artist to " + r.ViaArtistName + "."
		}
		return "Adjacent listen based on your affinity."
	default:
		if artist != "" {
			return "Recommended from " + artist + "."
		}
		return ""
	}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ── ratings + hides ─────────────────────────────────────────────────

func readRatings(ctx context.Context, db *sql.DB, userID string) ([]FeedRating, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT subject_type, subject_id, rating FROM ratings WHERE user_id = ?
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedRating
	for rows.Next() {
		var r FeedRating
		if err := rows.Scan(&r.SubjectType, &r.SubjectID, &r.Rating); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func readHides(ctx context.Context, db *sql.DB, userID string) ([]FeedHide, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT subject_type, subject_id FROM hides WHERE user_id = ?
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedHide
	for rows.Next() {
		var h FeedHide
		if err := rows.Scan(&h.SubjectType, &h.SubjectID); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// parseSQLiteTime mirrors the helper used elsewhere — sqlite DATETIME
// columns come back as TEXT and the driver doesn't always unmarshal into
// time.Time cleanly.
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
	return time.Time{}, fmt.Errorf("feed: unrecognized time format %q", s)
}
