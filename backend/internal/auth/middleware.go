package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type ctxKey int

const ctxUserIDKey ctxKey = 0

// WithUserID returns a context carrying the authenticated user ID.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxUserIDKey, userID)
}

// UserIDFromContext extracts the authenticated user ID, or "" if absent.
func UserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxUserIDKey).(string)
	return v
}

// RequireAuth wraps handlers that need an authenticated user. It looks up the
// session cookie, verifies and touches the session, and stashes the user ID
// in the request context. Unauthenticated requests get a JSON 401.
func RequireAuth(store *SessionStore, cookieCfg CookieConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(CookieName)
			if err != nil {
				writeUnauthorized(w)
				return
			}
			sess, err := store.Get(r.Context(), cookie.Value)
			if errors.Is(err, ErrNoSession) || errors.Is(err, ErrSessionExpired) {
				ClearCookie(w, cookieCfg)
				writeUnauthorized(w)
				return
			}
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			// Throttled touch so every authenticated request updates LRU
			// freshness without write-amplifying the sessions table.
			_ = store.TouchIfStale(r.Context(), sess.ID, sess.LastAccessedAt)

			ctx := WithUserID(r.Context(), sess.UserID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "unauthorized",
		"message": "login required",
	})
}
