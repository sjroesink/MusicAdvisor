package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/services/lbsimilar"
	"github.com/sjroesink/music-advisor/backend/internal/services/library"
	"github.com/sjroesink/music-advisor/backend/internal/services/listening"
	"github.com/sjroesink/music-advisor/backend/internal/services/mbrels"
	"github.com/sjroesink/music-advisor/backend/internal/services/releases"
	"github.com/sjroesink/music-advisor/backend/internal/services/toplists"
	"github.com/sjroesink/music-advisor/backend/internal/sse"
)

// SyncDeps is what the sync endpoints need. LibrarySync may be nil when the
// server boots without Spotify creds — in that case the endpoint returns a
// clear 503 rather than silently succeeding. The MB-only services (TopLists,
// Listening, Releases) are nil when MusicBrainz isn't configured and skip
// silently during trigger.
type SyncDeps struct {
	DB          *sql.DB
	Logger      *slog.Logger
	LibrarySync *library.Service
	TopLists    *toplists.Service
	Listening   *listening.Service
	Releases    *releases.Service
	LBSimilar   *lbsimilar.Service
	MBRels      *mbrels.Service
	Hub         *sse.Hub
}

// emit publishes a phase-lifecycle event. When the hub is nil (tests or
// degraded config) the call is a no-op.
func (d SyncDeps) emit(userID, phase, status string) {
	if d.Hub == nil {
		return
	}
	d.Hub.Publish(userID, sse.Event{
		Kind: "phase",
		Data: `{"phase":"` + phase + `","status":"` + status + `"}`,
	})
}

// TriggerSync kicks a full library sync for the authenticated user in a
// detached goroutine and returns 202 Accepted immediately. The caller can
// poll GET /api/sync/runs to see progress.
func TriggerSync(d SyncDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := auth.UserIDFromContext(r.Context())
		if userID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		if d.LibrarySync == nil {
			writeError(w, http.StatusServiceUnavailable, "sync_not_configured",
				"library sync disabled — check MA_SPOTIFY_CLIENT_ID / MA_USER_AGENT_CONTACT")
			return
		}

		// Detach from the request context so the sync keeps running after the
		// client response; cap with a generous ceiling so a stuck sync won't
		// block the worker forever.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			d.Logger.Info("background sync queued", "user_id", userID)
			d.emit(userID, "library", "started")

			libResult, err := d.LibrarySync.Sync(ctx, userID)
			if err != nil {
				d.Logger.Warn("background library sync failed", "user_id", userID, "err", err)
				d.emit(userID, "library", "failed")
				return
			}
			d.emit(userID, "library", libResult.Status)
			d.Logger.Info("library sync finished",
				"user_id", userID,
				"run_id", libResult.RunID,
				"status", libResult.Status,
				"artists_added", libResult.ArtistsAdded,
				"albums_added", libResult.AlbumsAdded,
				"unresolved", libResult.Unresolved,
				"errors", libResult.Errors,
				"duration_s", libResult.DurationMs/1000,
			)

			if d.TopLists != nil {
				d.emit(userID, "toplists", "started")
				topResult, err := d.TopLists.Sync(ctx, userID)
				if err != nil {
					d.emit(userID, "toplists", "failed")
					d.Logger.Warn("background toplists sync failed", "user_id", userID, "err", err)
				} else {
					d.emit(userID, "toplists", topResult.Status)
					d.Logger.Info("toplists sync finished",
						"user_id", userID,
						"run_id", topResult.RunID,
						"status", topResult.Status,
						"ranges", topResult.Ranges,
						"artists_done", topResult.ArtistsDone,
						"tracks_done", topResult.TracksDone,
						"unresolved", topResult.Unresolved,
						"errors", topResult.Errors,
						"duration_s", topResult.DurationMs/1000,
						"skipped_reason", topResult.SkippedReason,
					)
				}
			}

			if d.Listening != nil {
				d.emit(userID, "listening", "started")
				listenResult, err := d.Listening.Sync(ctx, userID)
				if err != nil {
					d.emit(userID, "listening", "failed")
					d.Logger.Warn("background listening sync failed", "user_id", userID, "err", err)
				} else {
					d.emit(userID, "listening", listenResult.Status)
					d.Logger.Info("listening sync finished",
						"user_id", userID,
						"run_id", listenResult.RunID,
						"status", listenResult.Status,
						"fetched", listenResult.Fetched,
						"inserted", listenResult.Inserted,
						"full", listenResult.PlayedFull,
						"skipped", listenResult.Skipped,
						"unresolved", listenResult.Unresolved,
						"errors", listenResult.Errors,
						"duration_ms", listenResult.DurationMs,
					)
				}
			}

			if d.Releases != nil {
				d.emit(userID, "releases", "started")
				relResult, err := d.Releases.Sync(ctx, userID)
				if err != nil {
					d.emit(userID, "releases", "failed")
					d.Logger.Warn("background releases sync failed", "user_id", userID, "err", err)
				} else {
					d.emit(userID, "releases", relResult.Status)
					d.Logger.Info("releases sync finished",
						"user_id", userID,
						"run_id", relResult.RunID,
						"status", relResult.Status,
						"artists", relResult.ArtistsScanned,
						"new", relResult.CandidatesNew,
						"updated", relResult.CandidatesUpdated,
						"errors", relResult.Errors,
						"duration_s", relResult.DurationMs/1000,
						"skipped_reason", relResult.SkippedReason,
					)
				}
			}

			if d.MBRels != nil {
				d.emit(userID, "mb-artist-rels", "started")
				mbResult, err := d.MBRels.Sync(ctx, userID)
				if err != nil {
					d.emit(userID, "mb-artist-rels", "failed")
					d.Logger.Warn("background mb-artist-rels sync failed", "user_id", userID, "err", err)
				} else {
					d.emit(userID, "mb-artist-rels", mbResult.Status)
					d.Logger.Info("mb-artist-rels sync finished",
						"user_id", userID,
						"run_id", mbResult.RunID,
						"status", mbResult.Status,
						"seeds", mbResult.SeedsScanned,
						"related", mbResult.RelatedDiscovered,
						"new", mbResult.CandidatesNew,
						"updated", mbResult.CandidatesUpdated,
						"errors", mbResult.Errors,
						"duration_s", mbResult.DurationMs/1000,
						"skipped_reason", mbResult.SkippedReason,
					)
				}
			}

			if d.LBSimilar != nil {
				d.emit(userID, "lb-similar", "started")
				lbResult, err := d.LBSimilar.Sync(ctx, userID)
				if err != nil {
					d.emit(userID, "lb-similar", "failed")
					d.Logger.Warn("background lb-similar sync failed", "user_id", userID, "err", err)
				} else {
					d.emit(userID, "lb-similar", lbResult.Status)
					d.Logger.Info("lb-similar sync finished",
						"user_id", userID,
						"run_id", lbResult.RunID,
						"status", lbResult.Status,
						"seeds", lbResult.SeedsScanned,
						"similar", lbResult.SimilarDiscovered,
						"new", lbResult.CandidatesNew,
						"updated", lbResult.CandidatesUpdated,
						"errors", lbResult.Errors,
						"duration_s", lbResult.DurationMs/1000,
						"skipped_reason", lbResult.SkippedReason,
					)
				}
			}

			d.emit(userID, "sync", "done")
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	}
}

// SyncRun is the shape of a row returned by GET /api/sync/runs.
type SyncRun struct {
	ID          int64      `json:"id"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	ItemsAdded  int        `json:"items_added"`
	Error       string     `json:"error,omitempty"`
	DurationMs  int64      `json:"duration_ms"`
}

// ListSyncRuns returns the most recent runs for the authenticated user.
func ListSyncRuns(d SyncDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := auth.UserIDFromContext(r.Context())
		if userID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}

		limit := 20
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}

		rows, err := d.DB.QueryContext(r.Context(), `
			SELECT id, kind, status, started_at, finished_at, items_added,
			       COALESCE(error, '')
			FROM sync_runs
			WHERE user_id = ?
			ORDER BY started_at DESC
			LIMIT ?
		`, userID, limit)
		if err != nil {
			d.Logger.Error("list sync_runs", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not list sync runs")
			return
		}
		defer rows.Close()

		out := make([]SyncRun, 0, limit)
		for rows.Next() {
			var run SyncRun
			var finishedAt sql.NullTime
			if err := rows.Scan(&run.ID, &run.Kind, &run.Status, &run.StartedAt,
				&finishedAt, &run.ItemsAdded, &run.Error); err != nil {
				d.Logger.Error("scan sync_runs", "err", err)
				writeError(w, http.StatusInternalServerError, "internal", "could not read sync runs")
				return
			}
			if finishedAt.Valid {
				t := finishedAt.Time
				run.FinishedAt = &t
				run.DurationMs = t.Sub(run.StartedAt).Milliseconds()
			}
			out = append(out, run)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}
