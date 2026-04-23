package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
)

// MeResponse is what /api/me returns.
type MeResponse struct {
	UserID       string                 `json:"user_id"`
	Spotify      *MeSpotify             `json:"spotify,omitempty"`
	Integrations map[string]Integration `json:"integrations"`
}

type MeSpotify struct {
	DisplayName string `json:"display_name,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
	Connected   bool   `json:"connected"`
	NeedsReconnect bool `json:"needs_reconnect,omitempty"`
}

type Integration struct {
	Connected      bool   `json:"connected"`
	NeedsReconnect bool   `json:"needs_reconnect,omitempty"`
	ExternalID     string `json:"external_id,omitempty"`
}

// Me returns the authenticated user's profile. Requires RequireAuth middleware.
func Me(db *sql.DB, users *user.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := auth.UserIDFromContext(r.Context())
		if userID == "" {
			// Should never happen if RequireAuth is in front, but guard anyway.
			writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}

		integrations, err := users.IntegrationsByUser(r.Context(), userID)
		if err != nil {
			logger.Error("me: integrations", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not load profile")
			return
		}

		resp := MeResponse{
			UserID:       userID,
			Integrations: make(map[string]Integration, len(integrations)),
		}
		for _, i := range integrations {
			resp.Integrations[i.Provider] = Integration{
				Connected:      true,
				NeedsReconnect: i.NeedsReconnect,
				ExternalID:     i.ExternalID,
			}
			if i.Provider == "spotify" {
				display, image := fetchSpotifyDisplay(r.Context(), db, userID)
				resp.Spotify = &MeSpotify{
					DisplayName:    display,
					ImageURL:       image,
					Connected:      true,
					NeedsReconnect: i.NeedsReconnect,
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// fetchSpotifyDisplay is a placeholder that will be fleshed out once we start
// caching profile snapshots. For now it returns empty strings so /api/me
// stays cheap and the frontend can fall back to generic labels.
func fetchSpotifyDisplay(_ context.Context, _ *sql.DB, _ string) (display, image string) {
	return "", ""
}
