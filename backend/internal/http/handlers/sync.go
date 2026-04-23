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
	"github.com/sjroesink/music-advisor/backend/internal/services/library"
)

// SyncDeps is what the sync endpoints need. LibrarySync may be nil when the
// server boots without Spotify creds — in that case the endpoint returns a
// clear 503 rather than silently succeeding.
type SyncDeps struct {
	DB          *sql.DB
	Logger      *slog.Logger
	LibrarySync *library.Service
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
			result, err := d.LibrarySync.Sync(ctx, userID)
			if err != nil {
				d.Logger.Warn("background sync failed", "user_id", userID, "err", err)
				return
			}
			d.Logger.Info("background sync finished",
				"user_id", userID,
				"run_id", result.RunID,
				"status", result.Status,
				"artists_added", result.ArtistsAdded,
				"albums_added", result.AlbumsAdded,
				"tracks_added", result.TracksAdded,
				"unresolved", result.Unresolved,
				"errors", result.Errors,
				"duration_ms", result.DurationMs,
			)
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
