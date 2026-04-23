// Package user owns account-identity operations: find-or-create from an
// external provider account, fetch integration state, revoke external
// tokens. Phase 2 only needs Spotify; the shape is provider-agnostic so
// Last.fm / ListenBrainz can plug in later.
package user

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/sjroesink/music-advisor/backend/internal/auth"
)

// Service encapsulates user-related DB writes.
type Service struct {
	db     *sql.DB
	cipher *auth.Cipher
	now    func() time.Time
}

func NewService(db *sql.DB, cipher *auth.Cipher) *Service {
	return &Service{db: db, cipher: cipher, now: time.Now}
}

// ExternalAccount describes what the OAuth callback provides.
type ExternalAccount struct {
	Provider        string
	ExternalID      string
	AccessToken     string
	RefreshToken    string
	TokenExpiresAt  time.Time
	Scopes          string
}

// UpsertByExternal looks up any user attached to the (provider, external_id)
// tuple. If found, it updates tokens and returns that user's ID. If not, it
// creates a new user row, inserts the external_accounts row, and returns the
// new user ID. Tokens are AES-GCM encrypted at rest.
func (s *Service) UpsertByExternal(ctx context.Context, a ExternalAccount) (string, error) {
	if a.Provider == "" || a.ExternalID == "" {
		return "", errors.New("user.UpsertByExternal: provider and external_id required")
	}
	encAccess, err := s.cipher.Encrypt([]byte(a.AccessToken))
	if err != nil {
		return "", err
	}
	encRefresh, err := s.cipher.Encrypt([]byte(a.RefreshToken))
	if err != nil {
		return "", err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var userID string
	err = tx.QueryRowContext(ctx, `
		SELECT user_id FROM external_accounts
		WHERE provider = ? AND external_id = ?
	`, a.Provider, a.ExternalID).Scan(&userID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		userID, err = s.createUser(ctx, tx)
		if err != nil {
			return "", err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO external_accounts (user_id, provider, external_id,
				access_token_enc, refresh_token_enc, token_expires_at, scopes,
				needs_reconnect, connected_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)
		`, userID, a.Provider, a.ExternalID, encAccess, encRefresh,
			a.TokenExpiresAt, a.Scopes, s.now().UTC())
		if err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		_, err = tx.ExecContext(ctx, `
			UPDATE external_accounts
			SET access_token_enc = ?, refresh_token_enc = ?,
			    token_expires_at = ?, scopes = ?, needs_reconnect = 0
			WHERE user_id = ? AND provider = ?
		`, encAccess, encRefresh, a.TokenExpiresAt, a.Scopes, userID, a.Provider)
		if err != nil {
			return "", err
		}
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return userID, nil
}

func (s *Service) createUser(ctx context.Context, tx *sql.Tx) (string, error) {
	id, err := newUserID()
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO users(id, created_at) VALUES (?, ?)`,
		id, s.now().UTC(),
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// AccessToken returns a decrypted, non-expired access token for the user,
// refreshing via the provided callback if necessary. The callback is passed
// in to keep this package free of provider dependencies; the signature is
// kept as an anonymous func so callers match Go's structural interface
// rules without needing a named type.
//
//	refresh(ctx, providerExternalID, currentRefreshToken) -> (newAccess, newRefresh, newExpiresAt, err)
//
// Returns ("", err) if there is no account or refresh fails.
func (s *Service) AccessToken(ctx context.Context, userID, provider string,
	refresh func(ctx context.Context, externalID, refreshToken string) (string, string, time.Time, error),
) (string, error) {
	var (
		externalID        string
		accessTokenEnc    []byte
		refreshTokenEnc   []byte
		tokenExpiresAtRaw sql.NullTime
		needsReconnect    int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT external_id, access_token_enc, refresh_token_enc,
		       token_expires_at, needs_reconnect
		FROM external_accounts
		WHERE user_id = ? AND provider = ?
	`, userID, provider).Scan(&externalID, &accessTokenEnc, &refreshTokenEnc,
		&tokenExpiresAtRaw, &needsReconnect)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoAccount
	}
	if err != nil {
		return "", err
	}
	if needsReconnect != 0 {
		return "", ErrNeedsReconnect
	}

	accessToken, err := s.cipher.Decrypt(accessTokenEnc)
	if err != nil {
		return "", err
	}

	// 60-second grace window so we refresh before a token expires mid-flight.
	if tokenExpiresAtRaw.Valid && s.now().Add(time.Minute).Before(tokenExpiresAtRaw.Time) {
		return string(accessToken), nil
	}

	refreshTok, err := s.cipher.Decrypt(refreshTokenEnc)
	if err != nil {
		return "", err
	}
	newAccess, newRefresh, newExpires, err := refresh(ctx, externalID, string(refreshTok))
	if err != nil {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE external_accounts SET needs_reconnect = 1 WHERE user_id = ? AND provider = ?`,
			userID, provider,
		)
		return "", err
	}
	newAccessEnc, err := s.cipher.Encrypt([]byte(newAccess))
	if err != nil {
		return "", err
	}
	newRefreshEnc, err := s.cipher.Encrypt([]byte(newRefresh))
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE external_accounts
		SET access_token_enc = ?, refresh_token_enc = ?, token_expires_at = ?
		WHERE user_id = ? AND provider = ?
	`, newAccessEnc, newRefreshEnc, newExpires, userID, provider)
	if err != nil {
		return "", err
	}
	return newAccess, nil
}

// IntegrationState is what /api/me returns about connected providers.
type IntegrationState struct {
	Provider       string
	ExternalID     string
	Scopes         string
	NeedsReconnect bool
	ConnectedAt    time.Time
}

func (s *Service) IntegrationsByUser(ctx context.Context, userID string) ([]IntegrationState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, external_id, scopes, needs_reconnect, connected_at
		FROM external_accounts WHERE user_id = ?
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntegrationState
	for rows.Next() {
		var i IntegrationState
		var needs int64
		if err := rows.Scan(&i.Provider, &i.ExternalID, &i.Scopes, &needs, &i.ConnectedAt); err != nil {
			return nil, err
		}
		i.NeedsReconnect = needs != 0
		out = append(out, i)
	}
	return out, rows.Err()
}

var (
	ErrNoAccount      = errors.New("user: no external account")
	ErrNeedsReconnect = errors.New("user: account needs reconnect")
)

func newUserID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// UUIDv4-shaped string without needing a dependency. Sets version=4 and
	// variant=10, so it's indistinguishable from a real UUIDv4 for storage.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:4]) + "-" +
		hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" +
		hex.EncodeToString(b[8:10]) + "-" +
		hex.EncodeToString(b[10:]), nil
}
