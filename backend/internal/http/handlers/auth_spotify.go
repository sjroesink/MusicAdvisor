package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
	"github.com/sjroesink/music-advisor/backend/internal/providers/spotify"
	"github.com/sjroesink/music-advisor/backend/internal/services/user"
)

// SpotifyAuthDeps wires what the Spotify auth handlers need. Anything nil is
// surfaced as a friendly 503 so the server still boots before the operator
// has registered a Spotify Developer App.
type SpotifyAuthDeps struct {
	DB         *sql.DB
	Logger     *slog.Logger
	Sessions   *auth.SessionStore
	CookieCfg  auth.CookieConfig
	Users      *user.Service
	Spotify    *spotify.Client // may be nil when creds are missing
	FrontendOK string          // redirect target after successful login
}

// SpotifyLogin begins an OAuth flow: generate state + PKCE, persist to
// oauth_states with a 5-minute TTL, redirect to Spotify.
func SpotifyLogin(d SpotifyAuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Spotify == nil {
			writeError(w, http.StatusServiceUnavailable, "spotify_not_configured",
				"Spotify client id/secret not configured on this server")
			return
		}
		verifier, challenge, err := spotify.NewPKCE()
		if err != nil {
			d.Logger.Error("spotify login: pkce", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not start login")
			return
		}
		state, err := spotify.NewState()
		if err != nil {
			d.Logger.Error("spotify login: state", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not start login")
			return
		}

		_, err = d.DB.ExecContext(r.Context(), `
			INSERT INTO oauth_states(state, code_verifier, expires_at)
			VALUES (?, ?, ?)
		`, state, verifier, time.Now().UTC().Add(5*time.Minute))
		if err != nil {
			d.Logger.Error("spotify login: insert oauth_states", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not start login")
			return
		}

		authorizeURL := d.Spotify.AuthorizeURL(state, challenge, spotify.DefaultScopes)
		http.Redirect(w, r, authorizeURL, http.StatusFound)
	}
}

// SpotifyCallback handles Spotify's redirect back with ?code=&state=. It
// validates state, exchanges the code for tokens, fetches /me, upserts the
// user, creates a session, sets the cookie, redirects to the frontend.
func SpotifyCallback(d SpotifyAuthDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Spotify == nil {
			writeError(w, http.StatusServiceUnavailable, "spotify_not_configured",
				"Spotify client id/secret not configured on this server")
			return
		}
		q := r.URL.Query()
		if spotifyErr := q.Get("error"); spotifyErr != "" {
			writeError(w, http.StatusBadRequest, "spotify_error", spotifyErr)
			return
		}
		state := q.Get("state")
		code := q.Get("code")
		if state == "" || code == "" {
			writeError(w, http.StatusBadRequest, "bad_request", "missing code or state")
			return
		}

		verifier, err := consumeOAuthState(r.Context(), d.DB, state)
		if err != nil {
			d.Logger.Warn("spotify callback: bad state", "err", err)
			writeError(w, http.StatusBadRequest, "invalid_state",
				"state token missing, reused, or expired; start over")
			return
		}

		tokens, err := d.Spotify.ExchangeCode(r.Context(), code, verifier)
		if err != nil {
			d.Logger.Warn("spotify callback: exchange", "err", err)
			writeError(w, http.StatusBadGateway, "exchange_failed",
				"could not exchange code with Spotify")
			return
		}

		me, err := d.Spotify.GetMe(r.Context(), tokens.AccessToken)
		if err != nil {
			d.Logger.Warn("spotify callback: /me", "err", err)
			writeError(w, http.StatusBadGateway, "me_failed", "could not fetch Spotify profile")
			return
		}

		userID, err := d.Users.UpsertByExternal(r.Context(), user.ExternalAccount{
			Provider:       "spotify",
			ExternalID:     me.ID,
			AccessToken:    tokens.AccessToken,
			RefreshToken:   tokens.RefreshToken,
			TokenExpiresAt: tokens.ExpiresAt,
			Scopes:         tokens.Scope,
		})
		if err != nil {
			d.Logger.Error("spotify callback: upsert user", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not create user")
			return
		}

		sess, err := d.Sessions.Create(r.Context(), userID, r.UserAgent())
		if err != nil {
			d.Logger.Error("spotify callback: session", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "could not create session")
			return
		}
		auth.SetCookie(w, sess, d.CookieCfg)

		dest := d.FrontendOK
		if dest == "" {
			dest = "/"
		}
		http.Redirect(w, r, dest, http.StatusFound)
	}
}

// Logout clears the session cookie and deletes the DB record.
func Logout(sessions *auth.SessionStore, cookieCfg auth.CookieConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(auth.CookieName)
		if err == nil {
			_ = sessions.Delete(r.Context(), cookie.Value)
		}
		auth.ClearCookie(w, cookieCfg)
		w.WriteHeader(http.StatusNoContent)
	}
}

// consumeOAuthState atomically fetches + deletes an oauth_states row. Returns
// the stored code_verifier on success. Any miss (unknown, expired, reused)
// returns an error.
func consumeOAuthState(ctx context.Context, db *sql.DB, state string) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var verifier string
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT code_verifier, expires_at FROM oauth_states WHERE state = ?`,
		state,
	).Scan(&verifier, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("state not found")
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_states WHERE state = ?`, state); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if time.Now().UTC().After(expiresAt) {
		return "", errors.New("state expired")
	}
	return verifier, nil
}

// writeError centralizes the JSON error shape used across the API.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": message,
	})
}

