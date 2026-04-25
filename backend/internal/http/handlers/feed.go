package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/providers/musicbrainz"
	"github.com/sjroesink/music-advisor/backend/internal/services/discover"
)

// discoverSources is the ordered list of candidate sources the Discover
// section merges. Order matters only for tie-breaking within equal scores
// — normally raw_score DESC decides the winner.
var discoverSources = []string{
	discover.SourceLBSimilar,
	discover.SourceMBArtistRel,
	discover.SourceMBSameLabel,
	discover.SourceLastfmSim,
}

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
	ID             string   `json:"id"`
	SubjectType    string   `json:"subject_type"`
	Artist         string   `json:"artist"`
	ArtistSpotifyID string  `json:"artist_spotify_id,omitempty"`
	Title          string   `json:"title"`
	Year           int      `json:"year,omitempty"`
	Date           string   `json:"date,omitempty"`
	Type           string   `json:"type"`
	Tracks         int      `json:"tracks,omitempty"`
	Length         string   `json:"length,omitempty"`
	Reason         string   `json:"reason"`
	Cover          string   `json:"cover,omitempty"`
	CoverArtURL    string   `json:"cover_art_url,omitempty"`
	SpotifyID      string   `json:"spotify_id,omitempty"`
	Score          float64  `json:"score"`
	Source         string   `json:"source"`
	Sources        []string `json:"sources,omitempty"`
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

		discoverCards, err := readDiscoverMerged(r.Context(), d.DB, userID, discoverSources)
		if err != nil {
			d.Logger.Error("feed: discover", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not load feed")
			return
		}
		resp.Discover = discoverCards

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
		`SELECT COUNT(*) FROM saved_artists WHERE user_id = $1`, userID).Scan(&artistCount); err != nil {
		return h, err
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM saved_albums WHERE user_id = $1`, userID).Scan(&albumCount); err != nil {
		return h, err
	}
	h.LibraryCount = artistCount + albumCount

	// Latest sync_runs row drives status + last_sync_at.
	var started sql.NullTime
	var status sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT started_at, status
		FROM sync_runs
		WHERE user_id = $1
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
			h.LastSyncAt = started.Time
		}
	}
	return h, nil
}

// ── cards ───────────────────────────────────────────────────────────

// readCards walks discover_candidates for one source, joins catalog tables,
// and formats each row into a FeedCard. Expired candidates are excluded
// via a native TIMESTAMPTZ comparison.
func readCards(ctx context.Context, db *sql.DB, userID, source string) ([]FeedCard, error) {
	now := time.Now().UTC()
	rows, err := db.QueryContext(ctx, `
		SELECT dc.subject_type, dc.subject_id, dc.raw_score, dc.reason_data,
		       COALESCE(a.title, '')        AS title,
		       COALESCE(a.release_date, '') AS release_date,
		       COALESCE(a.type, 'Album')    AS type,
		       COALESCE(a.track_count, 0)   AS tracks,
		       COALESCE(a.length_sec, 0)    AS length_sec,
		       COALESCE(ar.name, '')        AS artist_name,
		       COALESCE(a.spotify_id, '')   AS album_spotify_id,
		       COALESCE(ar.spotify_id, '')  AS artist_spotify_id
		FROM discover_candidates dc
		LEFT JOIN albums  a  ON a.mbid  = dc.subject_id AND dc.subject_type = 'album'
		LEFT JOIN artists ar ON ar.mbid = a.primary_artist_mbid
		WHERE dc.user_id = $1
		  AND dc.source  = $2
		  AND (dc.expires_at IS NULL OR dc.expires_at > $3)
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
			subjectType     string
			subjectID       string
			score           float64
			reasonRaw       string
			title           string
			releaseDate     string
			albumType       string
			trackCount      int
			lengthSec       int
			artistName      string
			albumSpotifyID  string
			artistSpotifyID string
		)
		if err := rows.Scan(&subjectType, &subjectID, &score, &reasonRaw,
			&title, &releaseDate, &albumType, &trackCount, &lengthSec, &artistName,
			&albumSpotifyID, &artistSpotifyID); err != nil {
			return nil, err
		}
		c := FeedCard{
			ID:              subjectID,
			SubjectType:     subjectType,
			Artist:          artistName,
			ArtistSpotifyID: artistSpotifyID,
			Title:           title,
			Type:            albumType,
			Tracks:          trackCount,
			Score:           score,
			Source:          source,
			SpotifyID:       albumSpotifyID,
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
		if subjectType == "album" {
			c.CoverArtURL = musicbrainz.CoverArtURL(subjectID, 500)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// readDiscoverMerged pulls every non-expired candidate from the given
// sources and collapses duplicate (subject_type, subject_id) pairs into a
// single card. The winner is the row with the highest raw_score; the
// other sources' names go into the returned card's Sources[]. Merging
// is cheap — we hit the index once per source and build the result in-
// memory, bounded at 50 distinct subjects.
func readDiscoverMerged(ctx context.Context, db *sql.DB, userID string, sources []string) ([]FeedCard, error) {
	type key struct{ t, id string }
	// Per-subject aggregator — holds the best-scoring row plus the union
	// of sources seen. Keep insertion order via `order`.
	type agg struct {
		card    FeedCard
		sources []string
		seen    map[string]bool
	}
	subjects := map[key]*agg{}
	var order []key

	for _, src := range sources {
		cards, err := readCards(ctx, db, userID, src)
		if err != nil {
			return nil, err
		}
		for _, c := range cards {
			k := key{t: c.SubjectType, id: c.ID}
			existing, ok := subjects[k]
			if !ok {
				existing = &agg{
					card:    c,
					sources: []string{src},
					seen:    map[string]bool{src: true},
				}
				subjects[k] = existing
				order = append(order, k)
				continue
			}
			if !existing.seen[src] {
				existing.seen[src] = true
				existing.sources = append(existing.sources, src)
			}
			// Keep whichever row scored highest — that's "the best reason
			// we surfaced this release" for the representative card.
			if c.Score > existing.card.Score {
				c.Sources = existing.sources
				existing.card = c
			}
		}
	}

	// Sort by score DESC (stable on insertion order to keep dedup
	// deterministic for tests).
	type scored struct {
		key  key
		item *agg
	}
	all := make([]scored, 0, len(order))
	for _, k := range order {
		all = append(all, scored{k, subjects[k]})
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].item.card.Score > all[j].item.card.Score
	})

	out := make([]FeedCard, 0, len(all))
	for _, s := range all {
		card := s.item.card
		if len(s.item.sources) > 1 {
			card.Sources = append([]string(nil), s.item.sources...)
		}
		out = append(out, card)
		if len(out) >= 50 {
			break
		}
	}
	return out, nil
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
		ViaArtistName    string  `json:"via_artist_name"`
		ReleaseDate      string  `json:"release_date"`
		PrimaryType      string  `json:"primary_type"`
		LBScore          float64 `json:"lb_score"`
		Relation         string  `json:"relation"`
		LabelName        string  `json:"label_name"`
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
	case "mb_artist_rels":
		if r.ViaArtistName != "" && r.Relation != "" {
			return humanRelation(r.Relation) + " " + r.ViaArtistName + "."
		}
		if r.ViaArtistName != "" {
			return "Connected to " + r.ViaArtistName + "."
		}
		return "Related to an artist you know."
	case "mb_same_label":
		if r.LabelName != "" && r.ViaArtistName != "" {
			return "Same label as " + r.ViaArtistName + " (" + r.LabelName + ")."
		}
		if r.LabelName != "" {
			return "Released on " + r.LabelName + "."
		}
		return "Same label as an artist you follow."
	case "lastfm_similar":
		if r.ViaArtistName != "" {
			return "Last.fm listeners of " + r.ViaArtistName + " also listen to this."
		}
		return "Last.fm similarity based on your affinity."
	default:
		if artist != "" {
			return "Recommended from " + artist + "."
		}
		return ""
	}
}

// humanRelation turns a MB relation-type into an English phrase.
func humanRelation(relType string) string {
	switch relType {
	case "member of band":
		return "Solo project of a member of"
	case "has member":
		return "Member's project from"
	case "collaboration", "collaborator":
		return "Collaborator with"
	case "supporting musician":
		return "Supporting musician for"
	case "performer", "vocal", "instrument":
		return "Performer with"
	default:
		return "Connected to"
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
		SELECT subject_type, subject_id, rating FROM ratings WHERE user_id = $1
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
		SELECT subject_type, subject_id FROM hides WHERE user_id = $1
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

