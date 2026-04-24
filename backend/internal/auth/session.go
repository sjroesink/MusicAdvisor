package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	CookieName         = "ma_session"
	SessionTTL         = 30 * 24 * time.Hour
	sessionIDSizeBytes = 32
)

var (
	ErrNoSession      = errors.New("auth: no session")
	ErrSessionExpired = errors.New("auth: session expired")
)

// Session is a server-side session record.
type Session struct {
	ID             string
	UserID         string
	ExpiresAt      time.Time
	LastAccessedAt time.Time
	UserAgent      string
	CreatedAt      time.Time
}

// SessionStore persists sessions. Backed by SQLite in production; tests can
// pass any *sql.DB.
type SessionStore struct {
	db *sql.DB
}

func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db}
}

// Create inserts a new session for userID and returns the generated ID.
func (s *SessionStore) Create(ctx context.Context, userID, userAgent string) (Session, error) {
	id, err := newSessionID()
	if err != nil {
		return Session{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(SessionTTL)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, expires_at, last_accessed_at, user_agent, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, userID, expires, now, userAgent, now)
	if err != nil {
		return Session{}, err
	}
	return Session{
		ID:             id,
		UserID:         userID,
		ExpiresAt:      expires,
		LastAccessedAt: now,
		UserAgent:      userAgent,
		CreatedAt:      now,
	}, nil
}

// Get fetches a session by ID. Returns ErrNoSession if missing,
// ErrSessionExpired if past expiry.
func (s *SessionStore) Get(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, expires_at, last_accessed_at, user_agent, created_at
		FROM sessions WHERE id = $1
	`, id)
	var sess Session
	err := row.Scan(&sess.ID, &sess.UserID, &sess.ExpiresAt, &sess.LastAccessedAt, &sess.UserAgent, &sess.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		return Session{}, ErrSessionExpired
	}
	return sess, nil
}

// TouchIfStale updates last_accessed_at at most once per minute per session.
// Called by the auth middleware on every authenticated request; the throttle
// avoids write amplification on high-traffic sessions.
func (s *SessionStore) TouchIfStale(ctx context.Context, id string, lastAccessed time.Time) error {
	if time.Since(lastAccessed) < time.Minute {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_accessed_at = $1 WHERE id = $2`,
		time.Now().UTC(), id,
	)
	return err
}

// Delete removes a session (logout).
func (s *SessionStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

// DeleteExpired prunes expired sessions. Called by a background job later.
func (s *SessionStore) DeleteExpired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE expires_at < $1`, time.Now().UTC(),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CookieConfig controls how session cookies are emitted.
type CookieConfig struct {
	Secure   bool
	Domain   string
	SameSite http.SameSite
}

// CookieFromBaseURL derives sensible defaults from an MA_BASE_URL.
func CookieFromBaseURL(baseURL string) CookieConfig {
	return CookieConfig{
		Secure:   strings.HasPrefix(strings.ToLower(baseURL), "https://"),
		SameSite: http.SameSiteLaxMode,
	}
}

// SetCookie writes the session cookie on w.
func SetCookie(w http.ResponseWriter, sess Session, cfg CookieConfig) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sess.ID,
		Path:     "/",
		Domain:   cfg.Domain,
		Expires:  sess.ExpiresAt,
		MaxAge:   int(time.Until(sess.ExpiresAt).Seconds()),
		Secure:   cfg.Secure,
		HttpOnly: true,
		SameSite: cfg.SameSite,
	})
}

// ClearCookie writes an expired cookie to log the user out.
func ClearCookie(w http.ResponseWriter, cfg CookieConfig) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		Domain:   cfg.Domain,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		Secure:   cfg.Secure,
		HttpOnly: true,
		SameSite: cfg.SameSite,
	})
}

func newSessionID() (string, error) {
	b := make([]byte, sessionIDSizeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
