package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/services/signal"
)

type SignalsDeps struct {
	DB      *sql.DB
	Logger  *slog.Logger
	Signals *signal.SQLStore
}

// signalRequest is the shape of the JSON body. Weight is accepted but
// ignored outside of trusted sources; the UI should rely on DefaultWeight.
type signalRequest struct {
	Kind        string `json:"kind"`
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Context     string `json:"context,omitempty"`
}

// uiKinds is the allow-list of event kinds a logged-in user is permitted to
// emit from the UI. Library/sync-derived signals (library_add, follow_add,
// top_rank, play_*) are written server-side only.
var uiKinds = map[signal.Kind]struct{}{
	signal.HeardGood:   {},
	signal.HeardBad:    {},
	signal.Dismiss:     {},
	signal.FilterClick: {},
	signal.OpenClick:   {},
}

var validSubjects = map[signal.SubjectType]struct{}{
	signal.SubjectArtist: {},
	signal.SubjectAlbum:  {},
	signal.SubjectTrack:  {},
	signal.SubjectLabel:  {},
	signal.SubjectTag:    {},
	signal.SubjectType_:  {},
}

// PostSignal records a UI-emitted event and returns the updated affinity
// score for the subject so the frontend can reflect the change without a
// round trip to /api/me or a feed refresh.
func PostSignal(d SignalsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := auth.UserIDFromContext(r.Context())
		if userID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}

		var req signalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
			return
		}
		kind := signal.Kind(req.Kind)
		subj := signal.SubjectType(req.SubjectType)
		if _, ok := uiKinds[kind]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_kind",
				"kind must be heard_good, heard_bad, dismiss, filter_click or open_click")
			return
		}
		if _, ok := validSubjects[subj]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_subject_type",
				"subject_type must be artist, album, track, label, tag or type")
			return
		}
		if req.SubjectID == "" {
			writeError(w, http.StatusBadRequest, "missing_subject_id", "subject_id is required")
			return
		}

		err := d.Signals.Append(r.Context(), signal.Event{
			UserID:      userID,
			Kind:        kind,
			SubjectType: subj,
			SubjectID:   req.SubjectID,
			Source:      signal.SourceUI,
			Context:     req.Context,
		})
		if err != nil {
			d.Logger.Warn("signal append failed",
				"user_id", userID, "kind", req.Kind,
				"subject_type", req.SubjectType, "subject_id", req.SubjectID,
				"err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not record signal")
			return
		}

		score, _ := affinityScore(r.Context(), d.DB, userID, subj, req.SubjectID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"kind":    req.Kind,
			"subject": map[string]string{"type": req.SubjectType, "id": req.SubjectID},
			"score":   score,
		})
	}
}

// affinityScore reads the current affinity score for the subject. Returns
// 0 if no row exists yet or if the subject type has no affinity table
// (currently: tag, type).
func affinityScore(ctx context.Context, db *sql.DB, userID string, subj signal.SubjectType, id string) (float64, error) {
	var q string
	switch subj {
	case signal.SubjectArtist:
		q = `SELECT score FROM artist_affinity WHERE user_id=? AND artist_mbid=?`
	case signal.SubjectAlbum:
		q = `SELECT score FROM album_affinity WHERE user_id=? AND album_mbid=?`
	case signal.SubjectTrack:
		q = `SELECT score FROM track_affinity WHERE user_id=? AND track_mbid=?`
	case signal.SubjectLabel:
		q = `SELECT score FROM label_affinity WHERE user_id=? AND label_mbid=?`
	default:
		return 0, nil
	}
	var score float64
	if err := db.QueryRowContext(ctx, q, userID, id).Scan(&score); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return score, nil
}
